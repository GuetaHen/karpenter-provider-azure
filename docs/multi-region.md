# Experimental multi-region provisioning

An `AKSNodeClass` can list physical Azure regions in `spec.regions`, so that one
Karpenter controller launches nodes in several regions for one AKS cluster. The
feature is experimental and off by default. An AKSNodeClass without
`spec.regions` behaves exactly as before, whether the feature is on or off. Unit
and acceptance tests do not prove Azure provisioning or the pod network of a
live cluster; see [Testing](#testing).

Terminology: elsewhere in this provider, *regional* means non-zonal (see
[design 0009](../designs/0009-regional-vm-provisioning.md)); this page says
*multi-region* for an AKSNodeClass with `spec.regions` and for its nodes.

## Supported boundary

The implemented path requires `PROVISION_MODE=aksscriptless`, Linux nodes,
`NETWORK_PLUGIN=none` and an explicitly empty `NETWORK_PLUGIN_MODE`. The
bring-your-own CNI must already support pod, Service, DNS, policy and node
connectivity across the configured regions, without IP forwarding on the node
NICs (see [Network contract](#network-contract)).

An AKSNodeClass with `spec.regions` reports `ValidationSucceeded=False`:

- with reason `MultiRegionProvisionModeUnsupported` in the `bootstrappingclient`,
  `aksmachineapi` and `aksmachineapiheaderbatch` provisioning modes. The AKS
  Machine API modes would need the Machine API to create a machine outside the
  cluster region, with its own subnet, identities and bootstrap data.
- with reason `MultiRegionUnsupportedConfiguration` with Azure CNI in any mode
  and with either dataplane. Azure CNI Overlay routes its pod address ranges
  only inside the cluster VNet, and carrying them to other regions would need
  changes to AKS-managed components, such as the AKS-managed Cilium
  configuration, which
  [AKS does not support](https://learn.microsoft.com/azure/aks/azure-cni-powered-by-cilium).
  Azure CNI with pod IP addresses from the node subnet depends on AKS-managed IP
  address management and masquerade rules that are not verified for nodes in
  other regions.

In these cases, and while the feature is off, the controller also refuses to
launch the NodeClaims of such an AKSNodeClass before any Azure write, even when
the AKSNodeClass has a stale `Ready` condition.

Disk encryption sets, capacity reservation groups, node public IPs, Windows and
node image version pinning are rejected with `spec.regions`. Disk and persistent
volume topology remain the responsibility of the CSI driver and the workload:
use `WaitForFirstConsumer` and region- and zone-aware storage classes, and
validate restore, attach, rescheduling and data locality before you run stateful
workloads on these nodes.

The nodes of an AKSNodeClass with `spec.regions`, including those in the
cluster region, join no load-balancer backend pool, get no NIC network security
group and carry `node.kubernetes.io/exclude-from-external-load-balancers=true`,
because the cluster's load balancers and network security group cannot serve
other regions. Exposing workloads on these nodes requires a separately
validated ingress or Service architecture.

All VMs and NICs stay in the cluster's node resource group, with the usual
deterministic names. Grant the controller the usual VM and NIC permissions
there, read and join permissions on the networks of the listed regions, and
permission to assign every identity in `--multi-region-node-identities`. The
location of the resource group is not the physical location of its VMs.

## Network contract

The customer owns the network of each region. The provider creates no VNets,
peerings, routes, NAT gateways, firewalls, registries, CNI agents or network
policies. Before it uses a region, it verifies the ARM location and subscription
of the subnet's VNet, that the subnet exists and that the subnet has a network
security group. These read-only checks cannot prove end-to-end connectivity.

For each region that hosts nodes:

- **Subnet.** An existing subnet of a VNet in that region, in the controller's
  subscription, with its own network security group and with addresses that
  overlap no other node subnet, pod range or Service range. The provider
  attaches no network security group to the NICs of these nodes.
- **Outbound traffic.** A NAT gateway on the subnet, shared by the nodes of the
  region. The nodes of each region leave through the NAT gateway of that region;
  this design does not route their outbound traffic back to the cluster region.
  If a destination allows the cluster's outbound traffic by IP tag, create the
  public IP addresses of the NAT gateway with that tag.
- **Cluster traffic.** Global VNet peering between the cluster VNet and the
  VNet of each listed region, and between every two of those VNets. Peering is
  not transitive, so the cluster VNet and n other VNets need n(n+1)/2 peerings.
  Traffic between peered VNets stays on the
  [Microsoft global network](https://learn.microsoft.com/azure/networking/microsoft-global-network).
  Allow the traffic that the cluster and the CNI need between the node and pod
  address ranges of all regions, in both directions, in every network security
  group on the path. Rules that are scoped by address range, not by port, stay
  valid when the CNI changes its ports.
  [Subnet peering](https://learn.microsoft.com/azure/virtual-network/how-to-configure-subnet-peering)
  can later limit the peerings to the node subnets.
- **Images.** A
  [geo-replica](https://learn.microsoft.com/azure/container-registry/container-registry-geo-replication)
  in the region of each container registry that the nodes pull from, so that
  image pulls stay in the region.
- **API server.** Every node must reach the API server. If the cluster limits
  API server access to authorized IP ranges, include the outbound addresses of
  every region.
  [API Server VNet Integration](https://learn.microsoft.com/azure/aks/api-server-vnet-integration)
  gives a private path, but it is not verified for nodes in other regions.
- **IP forwarding.** The NICs of these nodes keep IP forwarding off, as the NICs
  of all Karpenter nodes do. The CNI must carry pod traffic between regions
  without it, for example by encapsulation between node IP addresses. A CNI that
  routes pod addresses natively across VNets needs IP forwarding and
  user-defined routes, which this provider does not set up.

[Network-isolated clusters](https://learn.microsoft.com/azure/aks/concepts-network-isolated),
[VNet encryption](https://learn.microsoft.com/azure/virtual-network/virtual-network-encryption-overview)
and outbound traffic through a firewall in the cluster region are not covered.

## Configuration

Install the CRD from the same release as the controller. Existing NodeClasses
that omit `regions` keep their single-region providers, caches, API behavior and
hash. An empty list is not a valid explicit API configuration. Only listed
regions are eligible: the cluster region is **not** added implicitly.
`vnetSubnetID` and `regions` are mutually exclusive.

The feature is off by default. Start the controller with `--enable-multi-region`
(environment variable `ENABLE_MULTI_REGION=true`; with Helm, set it under
`controller.env`). While it is off, no multi-region registry, provider, cache
or ARM client is constructed, and a NodeClass that sets `regions` never
launches nodes: `ValidationSucceeded` and `SubnetsReady` are `False` with
reason `MultiRegionDisabled`, without any per-region or network call, and
launch-time validation rejects it before any Azure write.

```yaml
apiVersion: karpenter.azure.com/v1beta1
kind: AKSNodeClass
metadata:
  name: multi-region-overflow
spec:
  imageFamily: Ubuntu
  regions:
    - name: centralus
      vnetSubnetID: /subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network-central/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes
    - name: westus3
      vnetSubnetID: /subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network-west/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes
```

The normal Azure credentials, node identity, bootstrap token, API endpoint, DNS
IP, default subnet, node resource group and cluster options are still required.
The default subnet identifies the cluster; it is not substituted for any
`regions[].vnetSubnetID`.

While the controller refuses `regions`, because the feature is off or because
of an unsupported provisioning mode, network plugin or NodeClass setting, the
NodeClass cannot launch nodes, so Karpenter does not replace its existing nodes
for their placement either: they keep running until `regions` is removed or
accepted. In the AKS-managed add-on (node auto-provisioning), AKS sets the
controller options; unless AKS enables this feature in a supported
provisioning mode, an AKSNodeClass with `regions` behaves this way there.

To turn the feature off, first remove `regions` from every AKSNodeClass and let
Karpenter replace their nodes in the cluster region. If the feature is turned
off first, the nodes in other regions are left without a NodeClass that can
replace them.

### Home-first NodePools with overflow to other regions

Within one NodePool, Karpenter ranks the compatible offerings of all listed
regions by price and availability, so a cheaper remote region wins over the
cluster region. To prefer the cluster region, give a NodePool whose NodeClass
omits `regions` a higher weight, and let a lower-weight NodePool whose NodeClass
lists only other regions take what the cluster region cannot place:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: home
spec:
  weight: 100
  template:
    spec:
      nodeClassRef: {group: karpenter.azure.com, kind: AKSNodeClass, name: default}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: multi-region-overflow
spec:
  weight: 10
  template:
    spec:
      nodeClassRef: {group: karpenter.azure.com, kind: AKSNodeClass, name: multi-region-overflow}
      # Keep workloads that have not accepted cross-region latency, egress cost
      # and load-balancer exclusion off these nodes.
      taints:
        - {key: example.com/cross-region, value: "true", effect: NoSchedule}
```

Weights order NodePools, not offerings: a pod that the home NodePool can place
goes there, even when a remote region is cheaper. A pod reaches the overflow
NodePool only when it tolerates the taints of the overflow NodePool and the home
NodePool cannot place it, for example because of the limits or requirements of
the home NodePool, or because the cluster region has no capacity for it. A
launch that fails for lack of capacity marks the offering unavailable for a few
minutes; after that, new nodes launch in the cluster region again. Nodes that
the overflow NodePool launched stay until Karpenter disrupts them, for example
through consolidation or expiry, within the disruption budgets of their
NodePool. The nodes of the home NodePool keep the cluster's normal networking.

One controller must own both NodePools. Do not run a second Karpenter
controller or another autoscaler that can launch a node for the same pods.

### Topology domains

Nodes carry `topology.kubernetes.io/region` and `topology.kubernetes.io/zone`.
Zonal values are region-qualified (for example `eastus2-1`), but non-zonal VMs
use zone `0` in every region. Spread replicas across regions with a
`topologySpreadConstraints` entry on `topology.kubernetes.io/region`; a zone
constraint alone treats non-zonal nodes in different regions as one domain.
Karpenter only creates domains that the NodePool requirements and the listed
regions allow.

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

## Placement, failure isolation and lifecycle

Each physical region has its own SKU, pricing, quota, image and unavailable
offering state. A SKU retains its real Azure name; offerings carry both region
and zone, including the non-zonal value `0`. There is no cross-product of
unrelated regions and zones and no artificial remote-region price premium.
Selection uses the existing price/capacity ordering. To prefer the cluster
region, use [home-first NodePools](#home-first-nodepools-with-overflow-to-other-regions).

Before the first Azure mutation, the controller records the selected region
and subnet in the NodeClaim's `karpenter.azure.com/region-placement` annotation
using an optimistic-lock patch. Retries reuse it, even when another region
becomes cheaper. A conflicting patch, invalid network or incompatible updated
NodeClass stops the launch before Azure writes. Do not edit that annotation.
To choose a different region after a failed placement, recreate the NodeClaim;
the failed region's capacity cache then guides the new scheduling attempt.

`regions` is not part of the NodeClass hash. Adding, reordering or removing
regions, or recasing a subnet ID, does not replace nodes whose recorded region
and subnet are still listed. A NodeClaim drifts when its own region or subnet
is removed, when `regions` is removed from the NodeClass, or when `regions` is
added to a NodeClass that still has single-region nodes, but only while the
controller accepts `regions` (see [Configuration](#configuration)). When the
NodeClass no longer offers the NodeClaim's region, core's own drift check reports
`InstanceTypeNotFound`, which is how removing a region normally shows up. Core
runs that check only for NodeClaims older than an hour, and after a successful
check not again for 30 minutes, a wait that a controller restart resets. Until
it runs, and whenever the region is still offered, as for a changed subnet,
the provider reports `MultiRegionPlacementDrift`. Either way, core replaces
drifted nodes within the NodePool's disruption budgets, like any other drift.
Instance garbage collection lists the whole node resource group, so it also
deletes the VM of a removed region whose NodeClaim is gone.

A region can be removed and listed again without a restart. While it is not
listed, the controller makes no network, SKU, quota or image call for it and
keeps its per-region state in memory; only the region's public price list is
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
catalog has NodeClaims (reason `MultiRegionCatalogUnavailable`), even when its
subnet fails as well. Image resolution leaves out a failing
region that the status has no image for, so that the images of the other
regions keep refreshing; a region whose images are in use keeps them through a
failure.

Per-region state is never borrowed from another region. The first use of a
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
`karpenter_multiregion_inconsistent_instance_type_offerings_total`. The merged
catalog is reused until a region returns different results, and a successful
network validation is reused for one minute. Images are
resolved independently and retain region requirements through maintenance
updates and image drift detection. Transient image-read errors retain the
previous status image set rather than initiating mass drift.

VM, NIC, bootstrap and extension payloads use the selected location. NICs do
not reference the home-region load balancer or home-region NSG, and they
keep IP forwarding off. ARM VM location
is authoritative when reconstructing NodeClaim labels. Existing resource-group
discovery and NIC garbage collection cover all regions; deletion does not
depend on the NodeClass, per-region network validation or a successful quota
refresh.

## Node contract

These properties hold for every node of a NodeClass with `regions`. Kubelet
gets its labels through `--node-labels` and the NodePool's `taints` and
`startupTaints` through `--register-with-taints`. Karpenter adds the NodeClaim
labels at registration, including `karpenter.sh/nodepool`, the node class label
and `topology.kubernetes.io/region`, whose value is the placement region.

- The node carries `node.kubernetes.io/exclude-from-external-load-balancers=true`
  and, in `kubernetes.azure.com/kubelet-identity-client-id`, the kubelet identity
  of the placement region. Its other labels are those of a node of the cluster
  region with the same NodeClass settings.
- The NodeClaim carries the `karpenter.azure.com/region-placement` annotation
  from launch.
- The VM and its NIC are in the cluster's node resource group at the placement
  region, and the VM name is the node name.
- The primary IP configuration of the NIC is in the placement subnet, with no
  network security group, no load-balancer backend pools and IP forwarding off.
- The user-assigned identities of the VM are the `nodeIdentities` of the region,
  or `--node-identities` for a region without an entry.
- The bootstrap configuration has the placement region as its location, the
  placement subnet, VNet and VNet resource group, empty network security group
  and route table names, and the kubelet identity client ID of the region.
- The VM, NIC and extensions carry the tags of a node of the cluster region:
  `karpenter.azure.com_cluster`, `compute.aks.billing=linux`, the NodePool tag,
  the NodeClass `tags` and `--additional-tags`.

## Testing

`pkg/providers/multiregion` tests offering aggregation across regions, failure
isolation, image requirements, network checks, recorded placement, optimistic
conflicts, retries, deletion, refresh failures, cold starts, inconsistent SKUs,
catalog reuse and placement drift. The cloudprovider suite schedules pending
pods through the real provider with synthetic Azure APIs in two regions. It
covers NIC isolation, retry, discovery and deletion, home-first NodePools with
overflow to other regions, the refusals before any Azure write, and that an
AKSNodeClass without `regions` sends identical requests with the feature on or
off. None of these prove Azure provisioning or the pod network of a live
cluster.

The `MultiRegion` e2e suite runs against an existing cluster whose controller
runs with `ENABLE_MULTI_REGION=true` as its only autoscaler, with a
bring-your-own CNI and prepared networks in the listed regions. Without
`MULTI_REGION_TEST_REGIONS` it skips. It is not part of the E2E matrix: each
matrix entry creates a cluster without networks in other regions, in the
`aksmachineapi` provisioning mode by default, where `spec.regions` is refused.

```bash
export MULTI_REGION_TEST_REGIONS='[{"name":"centralus","vnetSubnetID":"<subnet-arm-id>"},{"name":"westus3","vnetSubnetID":"<subnet-arm-id>"}]'
# Set the usual test/README.md credentials, subscription, cluster and region options.
make e2etests TEST_SUITE=MultiRegion
```

It first checks that the VNets of the cluster and of every listed subnet are
peered in a full mesh. It then launches one node in each listed region from one
NodePool, checks the location, subnet, load-balancer backend pools, network
security group and IP forwarding of each VM and NIC in ARM, probes pod traffic
between every two regions through cluster DNS, and deletes the nodes.

## Options of this fork

This fork adds options that the upstream provider does not have. They are off
by default. See [Options of this fork](multi-region-fork.md).
