# Experimental multi-region provisioning

This fork extends the existing `karpenter.azure.com/v1beta1` `AKSNodeClass`.
One Karpenter controller and one NodePool can select real Azure locations.
In [coexistence mode](#coexistence-mode), the controller serves the same API as
`multiregion.io/v1beta1` and runs beside another Karpenter installation.
It is a provider foundation for isolated testing, **not production-ready managed
NAP support**. No live-cluster validation is implied by unit or acceptance tests.

## Supported boundary

The implemented path requires `PROVISION_MODE=aksscriptless` and one of two
network setups:

- `NETWORK_PLUGIN=none` and an explicitly empty `NETWORK_PLUGIN_MODE`. The
  bring-your-own CNI must already support pod, Service, DNS, policy, and node
  connectivity across the configured regions.
- Azure CNI Overlay (`NETWORK_PLUGIN=azure`, `NETWORK_PLUGIN_MODE=overlay`) with
  [`--multi-region-external-pod-network`](#azure-cni-overlay-with-an-external-pod-network),
  where an agent outside this provider carries the pod traffic of regional nodes.

Only Linux nodes are supported. Other managed Azure CNI configurations, both AKS
Machine API dispatch modes, and the bootstrapping service path are rejected
before Azure writes, even when a NodeClass has a stale Ready condition.

AKS owns the managed controller image and the Machine API's placement behavior.
Building the `ccp` variant does not grant the ability to replace managed NAP, add
Machine API regional placement, or provide a managed pod-network dataplane.
To run this fork beside another Karpenter installation in the same cluster,
such as AKS node auto-provisioning, use [coexistence mode](#coexistence-mode).
Never run two controllers that serve the same `AKSNodeClass` API group.

Bring an existing subnet in each selected physical region, in the controller's
subscription. Each subnet needs its own regional NSG, outbound connectivity,
non-overlapping addresses, and connectivity to the API server and every other
node subnet. The provider verifies the VNet's ARM location, subscription, subnet
existence and subnet NSG. It does **not** create VNets, peerings, routes, NAT
gateways, CNI agents, or network policies; these read-only checks cannot prove
end-to-end network connectivity.

All VMs/NICs stay in the existing node resource group with the existing
deterministic names. Grant the controller the normal VM/NIC permissions there,
read/join permissions on the regional networks, and permission to assign every
identity in `--multi-region-node-identities`. Resource-group location is not the
VM's physical location.

Disk encryption sets, capacity reservation groups, node public IPs, Windows,
and node-image-version pinning are rejected with `spec.regions`. Disk/PV
topology remains the responsibility of the CSI driver and workload: use
`WaitForFirstConsumer` and region/zone-aware storage classes, and independently
validate restore, attach, rescheduling and data-locality behavior before running
stateful workloads. This feature does not install a cross-region storage policy.
Nodes opted into this feature are excluded from external load balancers, even
when the selected region is the cluster region. Exposing workloads requires a
separately validated ingress/service architecture.

## Configuration

Install the CRD from the same fork revision as the controller. Existing
NodeClasses that omit `regions` retain their single-region providers, caches,
API behavior and hash. An empty list is not a valid explicit API configuration.
Only listed regions are eligible: the cluster's home region is **not** added
implicitly. `vnetSubnetID` and `regions` are mutually exclusive.

The feature is off by default. Start the controller with `--enable-multi-region`
(environment variable `ENABLE_MULTI_REGION=true`; with Helm, set it under
`controller.env`). While it is off, no regional registry, provider, cache or
ARM client is constructed, and a NodeClass that sets `regions` never launches
nodes: static validation reports `MultiRegionUnsupportedConfiguration`,
`SubnetsReady` is `False` with reason `MultiRegionDisabled`, and launch-time
validation rejects it before any Azure write.

```yaml
apiVersion: karpenter.azure.com/v1beta1
kind: AKSNodeClass
metadata:
  name: regional-workers
spec:
  imageFamily: Ubuntu
  regions:
    - name: eastus2
      vnetSubnetID: /subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network-east/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes
    - name: westus2
      vnetSubnetID: /subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network-west/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: regional-workers
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.azure.com
        kind: AKSNodeClass
        name: regional-workers
      requirements:
        - key: topology.kubernetes.io/region
          operator: In
          values: [eastus2, westus2]
        - key: kubernetes.io/arch
          operator: In
          values: [amd64]
        - key: karpenter.sh/capacity-type
          operator: In
          values: [on-demand]
        # Optional: non-zonal VMs only. "regional" is the Azure VM placement scope
        # (no availability zone), the only scope in regions without zones; it is
        # not what makes this NodePool multi-region.
        - key: karpenter.azure.com/placement-scope
          operator: In
          values: [regional]
      # Keep workloads that have not accepted cross-region latency, egress cost
      # and load-balancer exclusion off these nodes.
      taints:
        - key: example.com/cross-region
          value: "true"
          effect: NoSchedule
      # Add any startup taints required by the chosen BYO CNI.
```

The normal Azure credentials, node identity, bootstrap token, API endpoint,
DNS IP, default subnet, node resource group and cluster options are still
required. The default subnet identifies the home cluster; it is not substituted
for any `regions[].vnetSubnetID`. In coexistence mode, use
`apiVersion: multiregion.io/v1beta1` for the `AKSNodeClass` and
`group: multiregion.io` in each `nodeClassRef`.

### Price-first selection and home-first NodePools

Within one NodePool, Karpenter ranks every compatible offering in every listed
region by price and availability, so a cheaper remote region wins over the home
region. To prefer the home region, give a NodePool whose NodeClass omits
`regions` a higher weight, and let a lower-weight regional NodePool take what
the home region cannot place:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: home-first
spec:
  weight: 100
  template:
    spec:
      nodeClassRef: {group: karpenter.azure.com, kind: AKSNodeClass, name: default}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: regional-overflow
spec:
  weight: 10
  template:
    spec:
      nodeClassRef: {group: karpenter.azure.com, kind: AKSNodeClass, name: regional-workers}
      taints:
        - {key: example.com/cross-region, value: "true", effect: NoSchedule}
```

Weights order NodePools, not offerings: a pod that the home NodePool can place
goes there even when a remote region is cheaper. A pod reaches the regional
NodePool only when it tolerates the taint and the home NodePool cannot place it,
for example because of the home NodePool's limits or requirements.

### Topology domains

Nodes carry `topology.kubernetes.io/region` and `topology.kubernetes.io/zone`.
Zonal values are region-qualified (for example `eastus2-1`), but non-zonal VMs
use zone `0` in every region. Spread replicas across regions with a
`topologySpreadConstraints` entry on `topology.kubernetes.io/region`; a zone
constraint alone treats non-zonal nodes in different regions as one domain.
Karpenter only creates domains that the NodePool requirements and the listed
regions allow.

### Azure CNI Overlay with an external pod network

`--multi-region-external-pod-network` (`MULTI_REGION_EXTERNAL_POD_NETWORK=true`,
default `false`) accepts `regions` on Azure CNI Overlay. The controller does not
start with it unless `--enable-multi-region`, `NETWORK_PLUGIN=azure` and
`NETWORK_PLUGIN_MODE=overlay` are also set. The provider then bootstraps
regional nodes as the [node contract](#node-contract) lists and leaves their pod
network to an agent outside this provider: it writes no CNI configuration beyond
a home node's and creates no routes, peerings, tunnels or pod address ranges.
Overlay pod ranges belong to the cluster VNet, so a regional node pairs the
cluster's `VNET_GUID` in `kubernetes.azure.com/nodenetwork-vnetguid` with the
name of the subnet its NIC is in.

### Per-region node identities

Where an identity may only be attached to VMs in some regions,
`--multi-region-node-identities` (`MULTI_REGION_NODE_IDENTITIES`) gives each
listed region its own identities as a JSON object:

```json
{"westus2": {"nodeIdentities": ["/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/westus2-nodes"], "kubeletIdentityClientID": "11111111-2222-3333-4444-555555555555"}}
```

Region keys are lowercased. The controller does not start without
`--enable-multi-region`, or with unknown fields, a region listed twice, an empty
`nodeIdentities`, a value that is not a user-assigned identity resource ID or a
client ID, or an entry for the cluster region. For a VM in a region with an
entry, the entry replaces `--node-identities` and `--kubelet-identity-client-id`;
it is never added to them. It sets the VM's user-assigned identities, the kubelet
identity in the bootstrap configuration and the
`kubernetes.azure.com/kubelet-identity-client-id` label, and it is the goal state
of drift detection and in-place updates. A region without an entry uses the
global values.

### FIPS node images

With `USE_SIG=false`, a NodeClass with `fipsMode: FIPS` reports
`ImagesReady=False` with reason `SIGRequiredForFIPS`, unless the controller runs
with `--community-gallery-fips-images` (`COMMUNITY_GALLERY_FIPS_IMAGES=true`,
default `false`). With it, a FIPS NodeClass of `imageFamily: AzureLinux`
resolves its FIPS node images from the public Azure Linux community gallery,
separately in the cluster region and in every `regions` entry, the way non-FIPS
community images are resolved:

- Only FIPS image definitions are used: `V3gen2fips`, `V3fips` and
  `V3gen2arm64fips` for Azure Linux 3, `V2gen2fips` and `V2fips` for Azure
  Linux 2. Each gets its newest published version. Placeholder versions such as
  `0.0.0` and versions excluded from latest are never selected, and there is no
  fallback to a non-FIPS image.
- A definition that a region does not publish is skipped there, so instance
  types that only it serves (for example arm64) cannot launch in that region;
  restrict `kubernetes.io/arch` and `karpenter.azure.com/sku-hyperv-generation`
  in the NodePool if that matters. A listing that skipped a definition is
  cached for five minutes instead of three days.
- A definition that a region stops listing keeps its image there until it is
  listed again, so nodes on it do not drift and new nodes still get it; then
  the maintenance window governs its update like any other. The image is not
  kept once the NodeClass no longer selects the definition (for example after
  an image family change), once the region leaves `regions`, or during a
  pending update (a new NodeClass, a Kubernetes upgrade); the definition then
  returns at its newest version. A definition withdrawn from a region for good
  keeps its last image there until one of these happens, and launches that
  need it fail there once that image version is deleted.
- A region with none of the definitions sets `ImagesReady=False` with reason
  `FIPSImagesNotFound` and a message that names the region. Ready images are
  kept, so existing nodes do not drift, and once every region lists its FIPS
  images again the maintenance window still governs their update. If the
  window cannot be determined then, the images in use stay, `ImagesReady`
  becomes `True`, and the error is logged and retried. The images of a pending
  update (a new NodeClass, a Kubernetes upgrade) are dropped instead, so that
  the update completes then.
- Other image families report reason `CommunityGalleryFIPSImagesUnavailable`:
  the Ubuntu community gallery lists a FIPS definition whose only version is a
  placeholder.
- Nodes get `kubernetes.azure.com/fips_enabled=true`. Image version maintenance
  and image drift work as for non-FIPS community images: versions advance only
  while the node OS maintenance window is open. The controller reads the window
  with `get` on ConfigMap `upcoming-maintenance-window` in the namespace that
  `SYSTEM_NAMESPACE` names; the chart sets the controller's own namespace.
  Without `SYSTEM_NAMESPACE`, when the ConfigMap is missing or empty, or when it
  has neither `aksManagedNodeOSUpgradeSchedule-start` nor
  `aksManagedNodeOSUpgradeSchedule-end`, the window counts as open: versions
  advance as soon as a newer one is listed, and nodes on older versions drift.
  When the window cannot be determined (the read is forbidden, only one of the
  two entries is set, or a time does not parse), the images of a ready NodeClass
  do not change, not even for a region added to `regions`, until it can.
- The option has no effect with `USE_SIG=true`.

`ENABLE_FIPS=true` selects no images. It requires `fipsMode: FIPS` on every
NodeClass that this controller serves (reason `FIPSRequired` otherwise) and
refuses launches for any other NodeClass. Set it with
`COMMUNITY_GALLERY_FIPS_IMAGES=true` when every NodeClass must be FIPS.

The scriptless bootstrap has no FIPS setting: as in AKS node bootstrapping for
Linux, FIPS is a property of the node image. Only a live node can show that the
bootstrap completes on the FIPS image and that the kernel runs in FIPS mode
(`/proc/sys/crypto/fips_enabled` is `1`).

## Placement, failure isolation and lifecycle

Each physical region has its own SKU, pricing, quota, image and unavailable
offering state. A SKU retains its real Azure name; offerings carry both region
and zone, including the non-zonal value `0`. There is no cross-product of
unrelated regions and zones and no artificial remote-region price premium.
Selection uses the existing price/capacity ordering. There is no implicit
home-first placement policy.

Before the first Azure mutation, the controller records the selected region
and subnet in the NodeClaim's `karpenter.azure.com/region-placement` annotation
using an optimistic-lock patch. Retries reuse it, even when another region
becomes cheaper. A conflicting patch, invalid network or incompatible updated
NodeClass stops the launch before Azure writes. Do not edit that annotation.
To choose a different region after a failed placement, recreate the NodeClaim;
the failed region's capacity cache then guides the new scheduling attempt.

`regions` is not part of the NodeClass hash. Adding, reordering or removing
regions, or recasing a subnet ID, does not replace nodes whose recorded region
and subnet are still listed. A NodeClaim drifts with reason
`RegionalPlacementDrift` only when its own region or subnet is removed, when
`regions` is removed from the NodeClass, or when `regions` is added to a
NodeClass that still has single-region nodes.

Regional state is never borrowed from another region. The first use of a
region loads its SKUs and quota; later requests refresh SKUs after 12 hours and
quota after 10 minutes, retrying a failed call at most once per minute. A region
that has loaded once keeps serving its last successful SKU catalog when a
refresh fails, so core drift never reports its healthy NodeClaims as
`InstanceTypeNotFound`. Its quota is served until it is 15 minutes old and is
then cleared so that it fails open, exactly like the home region. A region that
has never loaded fails instance-type listing for the whole NodeClass instead of
returning a partial catalog. If a SKU's capacity, overhead or capabilities
differ between regions, the first listed region that offers it is
authoritative: the other region's offerings for that SKU are excluded, logged
once per change and counted in
`karpenter_regional_inconsistent_instance_type_offerings_total`. The merged
catalog is reused until a region returns different results, and a successful
network validation is reused for one minute. Images are
resolved independently and retain region requirements through maintenance
updates and image drift detection. Transient image-read errors retain the
previous status image set rather than initiating mass drift.

VM, NIC, bootstrap and extension payloads use the selected location. NICs do
not reference the home-region load balancer or home-region NSG. ARM VM location
is authoritative when reconstructing NodeClaim labels. Existing resource-group
discovery and NIC garbage collection cover all regions; deletion does not
depend on the NodeClass, regional network validation or a successful quota
refresh.

## Coexistence mode

Coexistence mode runs this controller beside another Karpenter installation in
the same cluster, such as AKS node auto-provisioning, that serves
`karpenter.azure.com` AKSNodeClasses. Enable it with the environment variable
`COEXISTENCE_MODE=true`. There is no flag and no build tag: a plain
`go build ./cmd/controller` binary supports it. The value is read once at
process start; an invalid value stops the controller.

In coexistence mode the controller:

- serves `AKSNodeClass` only as `multiregion.io/v1beta1` (plural
  `aksnodeclasses`). NodePools reference it with `group: multiregion.io`, and
  nodes carry `multiregion.io/aksnodeclass` instead of
  `karpenter.azure.com/aksnodeclass`. All other label, annotation, finalizer and
  tag domains are unchanged.
- manages only NodePools, NodeClaims and Nodes whose node class group is
  `multiregion.io`, and never writes another installation's objects or a
  `karpenter.azure.com` AKSNodeClass.
- never installs or changes a CRD, and waits through API discovery until
  `multiregion.io` is served. Install
  `pkg/apis/crds/coexistence/multiregion.io_aksnodeclasses.yaml` from the same
  revision. It serves the `v1beta1` schema of
  `karpenter.azure.com_aksnodeclasses.yaml` under the new group, and a unit test
  fails when the two differ. Both installations share the `karpenter.sh`
  NodePool and NodeClaim CRDs, whose installed schemas must be compatible with
  this revision.
- tags every VM, NIC and VM extension it creates with
  `karpenter.azure.com_owner=multiregion.io` and records the NodePool in
  `karpenter.azure.com_nodepool` instead of `karpenter.sh_nodepool`. Its Azure
  Resource Graph listings require both tags, and it refuses to adopt, update or
  delete a VM or NIC without the owner tag, so garbage collection, adoption,
  in-place updates and cleanup touch only resources it created. NodeClass and
  `--additional-tags` entries that would forge these tags or match another
  installation's listings are dropped.
- does not start with an AKS Machine API `PROVISION_MODE`, with
  `MANAGE_EXISTING_AKS_MACHINES=true`, or with the `NodeOverlay` or
  `CapacityBuffer` feature gate on, because both installations would reconcile
  those cluster-wide objects.

Give each `multiregion.io` AKSNodeClass a name that no `karpenter.azure.com`
AKSNodeClass has. A controller outside coexistence mode resolves a NodeClaim's
node class by kind and name only; once a same-named class exists, its in-place
update rewrites the tags of this controller's VMs and removes the owner tag.
Static validation therefore gets the metadata of a `karpenter.azure.com/v1beta1`
AKSNodeClass with the same name and sets `ValidationSucceeded=False` with reason
`ForeignNodeClassNameConflict` while one exists, or with reason
`ForeignNodeClassCheckForbidden` when it may not read that group. The check
repeats at least every five minutes. When the controller refuses to delete a VM
that lacks the owner tag, the NodeClaim keeps its finalizer and gets a
`VirtualMachineNotOwned` warning event; restore the tag on the VM, or delete the
VM, so that the NodeClaim can terminate.

Both installations see every pending pod. Taint the coexistence NodePools and
give their pods the toleration and a `multiregion.io/aksnodeclass` node
selector, so that only one installation can place each pod.

Beyond the default rules, coexistence mode needs these Kubernetes permissions:
`get`, `list`, `watch`, `patch` and `update` on `aksnodeclasses.multiregion.io`,
`patch` and `update` on its `status`, and `get` on
`aksnodeclasses.karpenter.azure.com`. It needs no permission on CRDs and no write
on `karpenter.azure.com` AKSNodeClasses. The Helm chart of this revision grants
these rules for the default group only.

## Node contract

These properties hold for every node of a NodeClass with `regions` on Azure CNI
Overlay with `--multi-region-external-pod-network`. Kubelet gets the labels below
through `--node-labels` and the NodePool's `taints` and `startupTaints` through
`--register-with-taints`. Karpenter adds the NodeClaim labels at registration,
including `karpenter.sh/nodepool`, the node class label and
`topology.kubernetes.io/region`.

| Label | Value |
| --- | --- |
| `kubernetes.azure.com/cluster` | the node resource group, normalized as for home nodes |
| `kubernetes.azure.com/role`, `kubernetes.azure.com/mode` | `agent`, `user` |
| `kubernetes.azure.com/kubelet-identity-client-id` | the kubelet identity of the placement region |
| `kubernetes.azure.com/network-subnet` | the name of the placement subnet |
| `kubernetes.azure.com/nodenetwork-vnetguid` | `VNET_GUID`, the cluster VNet |
| `kubernetes.azure.com/azure-cni-overlay` | `true` |
| `kubernetes.azure.com/podnetwork-type` | `overlay` |
| `kubernetes.azure.com/network-stateless-cni` | `true` from Kubernetes 1.34, otherwise `false` |
| `kubernetes.azure.com/ebpf-dataplane` | `cilium` when `NETWORK_DATAPLANE=cilium` |
| `kubernetes.azure.com/os-sku`, `os-sku-requested`, `os-sku-effective` | from the image family |
| `kubernetes.azure.com/fips_enabled` | `true` for a FIPS NodeClass |
| `kubernetes.azure.com/scalesetpriority`, `kubernetes.azure.com/priority` | `regular` for on-demand nodes |
| `kubernetes.azure.com/localdns-state` | `enabled` or `disabled` |
| `karpenter.sh/do-not-sync-taints` | `true` |
| `node.kubernetes.io/exclude-from-external-load-balancers` | `true` |

- The VM and its NIC are in the cluster's node resource group at the placement
  region, and the VM name is the node name.
- The NIC's primary IP configuration is in the placement subnet, with no NSG and
  no load-balancer backend pools.
- The VM's user-assigned identities are the region's `nodeIdentities`, or
  `--node-identities` for a region without an entry.
- A FIPS NodeClass boots the FIPS image of the placement region (see
  [FIPS node images](#fips-node-images)); its bootstrap configuration is the
  same as for other nodes.
- The bootstrap configuration has the placement region as its location, the
  placement subnet, VNet and VNet resource group, empty NSG and route table
  names, the region's kubelet identity client ID, and no CNI configuration, as
  on a home node of an overlay cluster.
- The VM, NIC and extensions are tagged with `karpenter.azure.com_cluster`
  (`CLUSTER_NAME`), `compute.aks.billing=linux`, the NodePool tag and, in
  coexistence mode, the owner tag, plus NodeClass `tags` and `--additional-tags`.

## Reproducible builds

Use the repository Go/toolchain versions. For the exact checked-out fork commit,
build **both** variants without publishing or changing a cluster:

```bash
revision="$(git rev-parse HEAD)"
epoch="$(git show -s --format=%ct "$revision")"
export SOURCE_DATE_EPOCH="$epoch" KO_DATA_DATE_EPOCH="$epoch"
export KO_GO_PATH=hack/go-crossbuild.sh
export KO_DOCKER_REPO=example.invalid/karpenter
mkdir -p artifacts
GOFLAGS="-ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version=$revision" \
  ko build -B --sbom none --platform=linux/amd64 --push=false \
  --tarball=artifacts/controller.tar ./cmd/controller
GOFLAGS="-ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version=$revision-aks -tags=ccp" \
  ko build -B --sbom none --platform=linux/amd64 --push=false \
  --tarball=artifacts/controller-aks.tar ./cmd/controller
trivy image --input artifacts/controller.tar --ignore-unfixed --exit-code 1
trivy image --input artifacts/controller-aks.tar --ignore-unfixed --exit-code 1
sha256sum artifacts/controller*.tar
```

Coexistence mode, the overlay opt-in and per-region identities are runtime
options that need no build tag. The `-aks`/`ccp` image is the managed build variant. Keep scans and the
maintainer-controlled release gates intact; a personal fork cannot publish to
the official AKS image repository. Use a digest-pinned image from the exact
tested commit for any separately authorized deployment.

## Validation and dedicated-cluster acceptance

`pkg/providers/regional` tests regional offering aggregation, lazy/default paths,
failure isolation, image requirements, network preflight, persisted placement,
optimistic conflicts, retries, deletion, refresh failures, cold starts,
inconsistent SKUs, catalog reuse, placement drift and allocation-free image
matching. The existing cloudprovider suite
adds pending-pod scheduling through the real provider with synthetic Azure APIs,
two physical-location payloads, NIC isolation, retry/discovery/deletion, and
stale-readiness rejection, the operator gate, refresh failure, cold start and
placement drift. API/hash and image maintenance tests extend the
existing suites. `pkg/coexistencesuite` runs controller and provider specs with
`COEXISTENCE_MODE=true`: other installations' NodePools, NodeClaims, Nodes, VMs
and NICs stay unchanged, the listing queries and the name check are exact, and
refusals are reported. The default build is also tested against
coexistence-tagged resources. These are **not proof** of Azure provisioning,
managed CNI, or the behavior of another installation's binary.

The existing integration suite includes an opt-in live test. On a separately
authorized, dedicated cluster running this exact fork, started with
`ENABLE_MULTI_REGION=true`, as its only autoscaler,
with two fully prepared BYO-CNI regional networks:

```bash
export MULTI_REGION_TEST_REGIONS='[{"name":"eastus2","vnetSubnetID":"<east-subnet-arm-id>"},{"name":"westus2","vnetSubnetID":"<west-subnet-arm-id>"}]'
# Set the usual test/README.md credentials, subscription, cluster and region options.
make e2etests TEST_SUITE=integration FOCUS='Multi-region subnets'
```

This test provisions one NodePool in two regions, checks ARM VM/NIC locations
and subnet/LB configuration rather than labels alone, waits for registered and
initialized nodes, probes pod traffic in both directions and cluster DNS,
then verifies NodeClaim/Node deletion and NIC cleanup. It skips unless the
explicit regional input is present. It does not create the cluster/networks or
replace a hosted controller. Dedicated managed-NAP deployment, sustained
network/policy/Service tests, storage tests, regional outage recovery, and live
Azure throttling/partial-failure validation remain separate acceptance gates.
