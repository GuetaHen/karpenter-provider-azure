# Experimental multi-region provisioning

This fork extends the existing `karpenter.azure.com/v1beta1` `AKSNodeClass`.
One Karpenter controller and one NodePool can select real Azure locations.
It is a provider foundation for isolated testing, **not production-ready managed
NAP support**. No live-cluster validation is implied by unit or acceptance tests.

## Supported boundary

The implemented path requires `PROVISION_MODE=aksscriptless`,
`NETWORK_PLUGIN=none`, and an explicitly empty `NETWORK_PLUGIN_MODE`. The
bring-your-own CNI must already support pod, Service, DNS, policy, and node
connectivity across the configured regions. Only Linux nodes are supported.
Managed Azure CNI (including Overlay), both AKS Machine API dispatch modes, and
the bootstrapping service path are rejected before Azure writes, even when a
NodeClass has a stale Ready condition.

AKS owns the managed controller image and the Machine API's placement behavior.
Building the `ccp` variant does not grant the ability to replace managed NAP, add
Machine API regional placement, or provide a managed pod-network dataplane.
Testing this fork as the **one main managed NAP controller** requires an
AKS-approved image/configuration deployment contract and a supported networking
path. Do not run a second autoscaler alongside managed NAP as a substitute.

Bring an existing subnet in each selected physical region, in the controller's
subscription. Each subnet needs its own regional NSG, outbound connectivity,
non-overlapping addresses, and connectivity to the API server and every other
node subnet. The provider verifies the VNet's ARM location, subscription, subnet
existence and subnet NSG. It does **not** create VNets, peerings, routes, NAT
gateways, CNI agents, or network policies; these read-only checks cannot prove
end-to-end network connectivity.

All VMs/NICs stay in the existing node resource group with the existing
deterministic names. Grant the controller the normal VM/NIC permissions there
and read/join permissions on the regional networks. Resource-group location is
not the VM's physical location.

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
for any `regions[].vnetSubnetID`.

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

The `-aks`/`ccp` image is the managed build variant. Keep scans and the
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
existing suites. These are **not proof** of Azure provisioning or managed CNI.

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
