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
  where a pod network agent, such as the one that the `karpenter` chart can run
  (off by default), carries the pod traffic of regional nodes.

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

On Azure CNI Overlay with the [pod network agent](#pod-network-agent), a request
that a node receives for a LoadBalancer or NodePort Service with
`externalTrafficPolicy: Cluster` fails when it is forwarded to an endpoint on a
regional node. Give these Services `externalTrafficPolicy: Local` with at least
one endpoint on a home node, keep their endpoints off regional nodes, or reach
them through an ingress controller. Clients inside the cluster are not affected.

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
network to an agent, such as the chart's [pod network agent](#pod-network-agent):
the controller writes no CNI configuration beyond a home node's and creates no
routes, peerings, tunnels or pod address ranges.
Overlay pod ranges belong to the cluster VNet, so a regional node pairs the
cluster's `VNET_GUID` in `kubernetes.azure.com/nodenetwork-vnetguid` with the
name of the subnet its NIC is in.

The opt-in changes nothing for a NodeClass without `regions`, in coexistence
mode too. Its nodes are ordinary nodes of the cluster region: on `vnetSubnetID`
or the cluster subnet, in the cluster's load-balancer backend pools, with the
cluster's network security group, route table and AKS-managed pod network.
An agent that supplies the pod network of regional nodes tells them apart by
`node.kubernetes.io/exclude-from-external-load-balancers=true`, which this
provider sets on every node of a NodeClass with `regions` and on no other node,
or by the `karpenter.azure.com/region-placement` annotation, which such a
NodeClaim carries from launch and its Node from registration. Do not set that
label in the template of a home NodePool.

### Pod network agent

The `karpenter` chart can run a pod network agent, built from this repository
for amd64 and arm64, that supplies the pod network of regional nodes on Azure
CNI Overlay with the AKS-managed Cilium dataplane. It is off by default.
`podNetworkAgent.enabled=true` runs it as a DaemonSet on every Linux node. The
DaemonSet tolerates every taint, because the agent must run on a node that still
carries the startup taint it removes. The agent needs
`--multi-region-external-pod-network`, the cluster pod CIDR in
`podNetworkAgent.podCIDR` and the ARM ID of the node resource group in
`podNetworkAgent.nodeResourceGroupID`. The node subnets of the cluster and of
every region must reach each other, with UDP 4789 allowed between them. In
`kube-system/cilium-config`, `enable-host-legacy-routing` must be `true` and
`cni-exclusive` must not be `true`: eBPF host routing bypasses the agent's
kernel routes, and an exclusive Cilium renames other CNI files. The agent
programs nothing on a node unless it can confirm both, nor on a node labelled
`kubernetes.azure.com/ebpf-host-routing`.

The release publishes no image of the agent. Build one from
`cmd/pod-network-agent` for both architectures, for example with
`KO_DOCKER_REPO=<registry> ko build --platform=linux/amd64,linux/arm64 ./cmd/pod-network-agent`,
and set `podNetworkAgent.image.repository` with `tag`, `digest` or both. The
image needs nothing but the agent's binary; `podNetworkAgent.command` sets the
command for an image that holds it beside other programs. The DaemonSet runs
with the priority class `system-node-critical`, resolves names with the node's
DNS settings, gives the agent a minute to finish a running pass when it stops,
and replaces its pods one node at a time without surge, so that two agents
never program one node. With `podNetworkAgent.apiServer.host` and
`podNetworkAgent.apiServer.port`, the agent reaches the API server there instead
of through the `kubernetes` Service, which a new node serves only once its
Cilium agent runs.

The agent never assigns pod ranges and does not read `Node.spec.podCIDRs`. A
node's range is the `/24` in `status.networkContainers[].primaryIP` of its Azure
CNI Overlay `NodeNetworkConfig` (`kube-system/<node name>`), accepted only while
that object belongs to the Node's current UID and holds exactly one network
container, of type `overlay`, whose `nodeIP` is the Node's `InternalIP`, and the
`/24` lies in the pod CIDR and overlaps no other node's range. The agent
pins each node's range, region, VM ID and IP in memory when it accepts them; a
later change removes the node as a peer. A node is regional when it is
registered, carries the `karpenter.azure.com/region-placement` annotation, and
exactly one NodeClaim of this controller's node class group has its provider ID
and a placement that matches the VM's location and the node's region label.
Before it programs its own node, the agent also requires instance metadata to
report the Node's `systemUUID` as VM ID, ignoring case and either as it is or
with its first three groups byte-swapped, as Linux reports it on Generation 1
VMs, its `InternalIP` as private IP, and the subscription and resource group of
both `podNetworkAgent.nodeResourceGroupID` and the Node's provider ID.

On every node the agent programs a VXLAN tunnel on UDP 4789 with MTU 1400,
whose VNI derives from the node resource group ID. The VXLAN device is a port
of a bridge that holds the node's transit address as a `/32`: the node with the
n-th `/24` of the pod CIDR, counting from 0, gets the n-th address of
`podNetworkAgent.transitPrefix` (default `169.254.64.0/18`, enough for pod
CIDRs up to `/10`), and its bridge gets the MAC address `02:00` followed by the
four bytes of that address. For each peer the agent adds a permanent neighbor
entry, a static forwarding entry to the peer's node IP, a route to the peer's
transit address, and a route to the peer's `/24` through that address with the
node's own transit address as source, so nothing relies on ARP or flooding. A
connection that a node itself opens to a pod on a peer therefore carries
the node's transit address, which Cilium treats as `world`: a NetworkPolicy
that admits only cluster or node sources does not admit it. The agent accepts
UDP 4789 only from peers' node IPs and sets TCP MSS 1360 on SYNs into the
tunnel. It does not start when the pod CIDR overlaps `169.254.0.0/16`, or when
the transit prefix is not inside `169.254.0.0/16`, has host bits, is too small
for the pod CIDR or contains `169.254.169.254`. On every pass it changes nothing
on the node while another VXLAN device uses UDP 4789, a device with one of its
names is not its own, or a host address that it did not assign lies in the
transit prefix; what it programmed before stays. If it cannot finish creating
one of its devices, it deletes the device again. When it is stopped, it
finishes a running pass first; only if it is killed or crashes between
creating a device and marking it as its own does the device stay unmarked,
and the agent then refuses the node until the device is deleted or the node
restarts. A home node's peers are the regional nodes, and its CNI
configuration stays as AKS wrote it. A regional node's peers are all other
nodes, and there the agent also:

- writes a CNI configuration that sorts before every other file in
  `/etc/cni/net.d`: the `cilium-cni` plugin with `host-local` IPAM over the
  node's `/24` (addresses `.2` to `.254`, gateway `.1`), chained with the
  `tuning` plugin for MTU 1400;
- masquerades pod traffic that leaves through the node's own interface;
- drops forwarded traffic to `169.254.169.254`, so that pods cannot reach
  instance metadata.

A regional node offers 253 pod addresses, so a NodeClass that sets `maxPods`
above 253 is not supported on regional nodes.

The agent writes that CNI file once the node's Cilium agent is healthy, and
only while `/etc/cni/net.d` holds no other CNI configuration: pods started with
another configuration could hold addresses that `host-local` does not know. On
AKS, Azure CNS writes the CNI configuration of the Cilium dataplane only after
the Azure host reports the node's network container as programmed, and hands
out no pod address until then. Once the agent's file is in place, a later CNI
configuration does not matter, and an earlier one makes the node not ready.

The agent re-checks a peer on every change of its Node, NodeClaim or
`NodeNetworkConfig`, re-validates everything every 60 seconds, and removes a
peer's route as soon as a check fails. It marks its devices with an alias,
keeps its rules in its own nftables table and its routes under its own
protocol and metric, and never adopts or overwrites a device, route or CNI
file that it did not create. When the agent stops, it leaves what it programmed
in place: existing peers keep working, and the agent adds new peers and removes
departed ones when it runs again.

Every NodePool whose NodeClass lists regions sets the agent's startup taint,
whose key is `podNetworkAgent.startupTaintKey`:

```yaml
spec:
  template:
    spec:
      startupTaints:
        - key: karpenter.azure.com/regional-pod-network-not-ready
          effect: NoSchedule
```

With the agent enabled, the chart passes the key to the controller as
`--multi-region-pod-network-startup-taint`
(`MULTI_REGION_POD_NETWORK_STARTUP_TAINT`, default empty; do not set it in
`controller.env` as well). The controller then
refuses to launch a NodeClaim of a NodeClass with `regions`, before any write,
unless its `startupTaints` hold a `NoSchedule` or `NoExecute` taint with that
key. The agent removes the taint from its own node once the node no longer
carries `karpenter.sh/unregistered`, its checks pass and the tunnel is
programmed, and on a regional node also the node's Cilium agent is healthy
(`http://127.0.0.1:9879/healthz` with the header `brief: true`) and the CNI file
is in place. It puts the taint back only on a node whose NodeClaim, of this
controller's node class group, carries the
`karpenter.azure.com/region-placement` annotation, when any of that fails
later, only on the Node with the UID it checked and unless that Node is being
deleted. Its readiness probe reports the same conditions, apart from the
registration. A regional node that never becomes ready stays tainted until its
NodeClaim is deleted; this provider has no repair policy for it.

A home NodePool may set the same startup taint, so that its new nodes take pods
only once the agent has programmed the tunnel; the controller requires the
taint only for NodeClaims of a NodeClass with `regions`. New nodes of such a
pool take no pods while the agent cannot run, so roll the agent out before
setting the taint. Adding it to an existing NodePool replaces the pool's nodes,
because `startupTaints` is part of the NodePool's hash. Do not set it on a
Windows NodePool, where the agent does not run.

The agent runs as root with host networking but unprivileged: only the
`NET_ADMIN` capability, no privilege escalation, a read-only root file system
and the `RuntimeDefault` seccomp profile, with the host's `/etc/cni/net.d`
writable and `/opt/cni/bin` read-only. Its namespace needs the `privileged` Pod
Security level. It holds no Azure credentials. It may get, list and watch
Nodes and NodeClaims, patch Nodes, and in `kube-system` get, list and watch
`nodenetworkconfigs.acn.azure.com` and get the `cilium-config` ConfigMap. A
ValidatingAdmissionPolicy, which the chart installs with the agent unless
`podNetworkAgent.admissionPolicy.enabled=false`, lets it patch only its own
Node and only the startup taint. It recognizes the agent's Node by the node name
in the agent's service account token, which Kubernetes adds from version 1.30.

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
- A definition that a region stops listing keeps its image there, at its
  place in the image priority order, until it is listed again. Nodes on it do
  not drift, and new nodes get it as before: while the Gen2 definition is
  skipped, a size that supports both Hyper-V generations still gets the Gen2
  image, not the Gen1 image. Then the maintenance window governs its update
  like any other. The image is not kept once the NodeClass no longer selects
  the definition (for example after an image family change), once the region
  leaves `regions`, or during a
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
  `SYSTEM_NAMESPACE` names. The chart sets it to `settings.systemNamespace`, by
  default the controller's own namespace, and grants that `get` there. AKS
  publishes the ConfigMap in `kube-system`.
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
NodeClass that still has single-region nodes. Core replaces drifted nodes within
the NodePool's disruption budgets, so removing a region replaces its nodes like
any other drift. Instance garbage collection lists the whole node resource
group, so it also deletes the VM of a removed region whose NodeClaim is gone.

A region can be removed and listed again without a restart. While it is not
listed, the controller makes no network, SKU, quota or image call for it and
keeps its regional state in memory; only the region's public price list is
still refreshed every 12 hours. A launch that recorded the region before its
removal fails before any Azure write, and core deletes that NodeClaim after its
launch timeout. Delete the region's network only when its subnet has no IP
configurations left: Azure refuses to delete a subnet that NICs still use, and
removing the peering earlier cuts the pod network of the region's remaining
nodes. When the region is listed again, the next status check tests its subnet
again. Failed checks are never cached, so the region reports
`RegionsUnavailable` until its network, with an NSG on the subnet, exists again,
and then serves on it, whether the subnet ID is the same or new.
`MULTI_REGION_NODE_IDENTITIES` is read at start: keep a removed region's
identities while it may return, or restart the controller after changing its
entry.

A failing region does not block the others. Every three minutes the subnet
status check tests each listed region: its SKU catalog, its subnet and, once
the NodeClass status lists node images, an image for it. A failing region gets
no new nodes. Its instance types stay in the catalog with unavailable
offerings, so core drift still finds the instance types of its NodeClaims; a
region that has never loaded and has no NodeClaims is left out. `SubnetsReady`
stays `True` with reason `RegionsUnavailable` and names the failing regions,
and a region serves again once it passes. The NodeClass is not ready only while
every listed region fails, or while a region that has never loaded its SKU
catalog has NodeClaims (reason `RegionalCatalogUnavailable`), even when its
subnet fails as well. Image resolution leaves out a failing
region that the status has no image for, so that the images of the other
regions keep refreshing; a region whose images are in use keeps them through a
failure.

Regional state is never borrowed from another region. The first use of a
region loads its SKUs and quota; later requests refresh SKUs after 12 hours and
quota after 10 minutes, retrying a failed call at most once per minute. A region
that has loaded once keeps serving its last successful SKU catalog when a
refresh fails, so core drift never reports its healthy NodeClaims as
`InstanceTypeNotFound`. Its quota is served until it is 15 minutes old and is
then cleared so that it fails open, exactly like the home region. A region that
has never loaded fails instance-type listing for the whole NodeClass instead of
returning a partial catalog while it has NodeClaims (see above). If a SKU's
capacity, overhead or capabilities
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
  revision, for example with the `karpenter-coexistence-crd` chart as shown
  below. It serves the `v1beta1` schema of
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
  `MANAGE_EXISTING_AKS_MACHINES=true`, or with the `CapacityBuffer` feature
  gate on, because both installations would reconcile those cluster-wide
  objects.
- applies NodeOverlays to its own NodePools when the `NodeOverlay` feature gate
  is on, as its own validation decides, but never writes their status: the
  other installation validates the same overlays and owns it. It logs an
  overlay that fails its validation.

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
`aksnodeclasses.karpenter.azure.com`. It needs no permission on CRDs, no write
on `karpenter.azure.com` AKSNodeClasses and no write on `nodeoverlays/status`,
and the chart grants none of these in coexistence mode. The Helm charts install
coexistence mode by values and `--skip-crds`:

1. Install or upgrade the `karpenter-coexistence-crd` chart from
   `charts/karpenter-coexistence-crd` in this repository; the release script
   publishes only the `karpenter` and `karpenter-crd` charts. The chart holds
   only the `multiregion.io` CRD, which Helm applies on install and on every
   upgrade. The CRD carries `helm.sh/resource-policy: keep`: uninstalling the
   release, or upgrading to a chart without the CRD, leaves the CRD and every
   `multiregion.io` AKSNodeClass in place; delete the CRD yourself to remove
   them. When the CRD already exists without the Helm ownership of this
   release, because it was applied from its file or the keep policy left it
   behind under another release name or namespace, Helm refuses the install
   with invalid ownership metadata. Then give the CRD the label
   `app.kubernetes.io/managed-by=Helm` and the annotations
   `meta.helm.sh/release-name` and `meta.helm.sh/release-namespace` of the new
   release, or install with `--take-ownership` (Helm 3.17 or later). Helm 4
   applies server-side and reports a conflict where kubectl last wrote a field
   that the chart changes, on this install or on a later upgrade; pass
   `--force-conflicts`, and after a failed install use
   `helm upgrade --install`. Do not install the `karpenter-crd` chart for this
   controller: its five CRDs belong to the other installation.
2. Install or upgrade the `karpenter` chart with `--skip-crds`,
   `coexistence.enabled=true` and `webhook.enabled=false`. It sets
   `COEXISTENCE_MODE=true` and grants the rules above instead of those of the
   default group. It refuses to render with the webhooks enabled, whose rules
   update CRDs that both installations share. Without `--skip-crds`,
   `helm install` creates whichever of the five CRDs in `charts/karpenter/crds`
   the cluster lacks: NodePool, NodeClaim, NodeOverlay, the
   `karpenter.azure.com` AKSNodeClass and CapacityBuffer. Helm never upgrades or
   deletes the CRDs of that directory.

Give the release a name and a namespace of its own, so that its cluster-scoped
RBAC objects and its leader election lease differ from those of the other
installation, and keep the default `LEADER_ELECTION_NAME`: the chart's Role
allows lease updates only under that name.

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
