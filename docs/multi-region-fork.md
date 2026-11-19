# Options of this fork

This fork adds options to [experimental multi-region provisioning](multi-region.md).
The upstream provider does not have these options. Each option is off by
default. With all options off, the controller behaves as the upstream provider.

| Option | Environment variable | Section |
| --- | --- | --- |
| `--multi-region-external-pod-network` | `MULTI_REGION_EXTERNAL_POD_NETWORK` | [Azure CNI Overlay with an external pod network](#azure-cni-overlay-with-an-external-pod-network) |
| `--multi-region-pod-network-startup-taint` | `MULTI_REGION_POD_NETWORK_STARTUP_TAINT` | [Pod network agent](#pod-network-agent) |
| `--community-gallery-fips-images` | `COMMUNITY_GALLERY_FIPS_IMAGES` | [FIPS node images](#fips-node-images) |
| none | `COEXISTENCE_MODE` | [Coexistence mode](#coexistence-mode) |

The pod network agent needs changes to the AKS-managed Cilium configuration.
AKS documents such changes as not supported, see
[Azure CNI Powered by Cilium](https://learn.microsoft.com/azure/aks/azure-cni-powered-by-cilium).
Thus the agent is not a networking path for the upstream provider or for the
AKS-managed add-on.

## Azure CNI Overlay with an external pod network

`--multi-region-external-pod-network` (`MULTI_REGION_EXTERNAL_POD_NETWORK=true`,
default `false`) accepts `regions` on Azure CNI Overlay. The controller does not
start with it unless `--enable-multi-region`, `NETWORK_PLUGIN=azure` and
`NETWORK_PLUGIN_MODE=overlay` are also set. The provider then bootstraps
multi-region nodes as the [node contract](multi-region.md#node-contract) lists and leaves their pod
network to an agent, such as the chart's [pod network agent](#pod-network-agent):
the controller writes no CNI configuration beyond a home node's and creates no
routes, peerings, tunnels or pod address ranges.
Overlay pod ranges belong to the cluster VNet, so a multi-region node pairs the
cluster's `VNET_GUID` in `kubernetes.azure.com/nodenetwork-vnetguid` with the
name of the subnet its NIC is in.

The opt-in changes nothing for a NodeClass without `regions`, in coexistence
mode too. Its nodes are ordinary nodes of the cluster region: on `vnetSubnetID`
or the cluster subnet, in the cluster's load-balancer backend pools, with the
cluster's network security group, route table and AKS-managed pod network.
An agent that supplies the pod network of multi-region nodes tells them apart by
`node.kubernetes.io/exclude-from-external-load-balancers=true`, which this
provider sets on every node of a NodeClass with `regions` and on no other node,
or by the `karpenter.azure.com/region-placement` annotation, which such a
NodeClaim carries from launch and its Node from registration. Do not set that
label in the template of a home NodePool.

## Pod network agent

The `karpenter` chart can run a pod network agent, built from this repository
for amd64 and arm64, that supplies the pod network of multi-region nodes on Azure
CNI Overlay with the AKS-managed Cilium dataplane. It is off by default.
`podNetworkAgent.enabled=true` runs it as a DaemonSet on every Linux node except
virtual nodes (label `type=virtual-kubelet`), as the AKS DaemonSets do. The
DaemonSet tolerates every taint, because the agent must run on a node that still
carries the startup taint it removes. The agent needs
`--multi-region-external-pod-network`, the cluster pod CIDR in
`podNetworkAgent.podCIDR` and the ARM ID of the node resource group in
`podNetworkAgent.nodeResourceGroupID`. The node subnets of the cluster and of
every region must reach each other, with UDP 4789 allowed between them. In
`kube-system/cilium-config`, `enable-host-legacy-routing` must be set to `true`
and `cni-exclusive` to `false`; an unset key does not count. eBPF host routing
bypasses the agent's kernel routes, and an exclusive Cilium renames other CNI
files. The agent programs nothing on a node unless it can confirm both, nor on
a node labelled
`kubernetes.azure.com/ebpf-host-routing`.

The release publishes no image of the agent. Build one from
`cmd/pod-network-agent` for both architectures, for example with
`KO_DOCKER_REPO=<registry> ko build --platform=linux/amd64,linux/arm64 ./cmd/pod-network-agent`,
and set `podNetworkAgent.image.repository` with `tag`, `digest` or both. The
image needs nothing but the agent's binary; `podNetworkAgent.command` sets the
command for an image that holds it beside other programs. The DaemonSet runs
with the priority class `system-node-critical`, resolves names with the node's
DNS settings, gives the agent a minute to finish a running pass when it stops,
and replaces its pods without surge, so that two agents never program one node,
`podNetworkAgent.maxUnavailable` at a time, one by default. With
`podNetworkAgent.apiServer.host` and `podNetworkAgent.apiServer.port`, the agent
reaches the API server there instead of through the `kubernetes` Service, which
a new node serves only once its Cilium agent runs.

An agent pod is ready only while its node meets the conditions under which the
agent removes the startup taint, described below. On a node that the agent
refuses for good, the pod stays unready and counts against
`podNetworkAgent.maxUnavailable`, so that many such nodes stall every later
rollout of the DaemonSet: `kubectl rollout status` on it does not finish, and
the log of an unready agent pod says why the agent refuses its node. Fix or
replace those nodes, or raise `podNetworkAgent.maxUnavailable`.

The agent never assigns pod ranges and does not read `Node.spec.podCIDRs`. A
node's range is the `/24` in `status.networkContainers[].primaryIP` of its Azure
CNI Overlay `NodeNetworkConfig` (`kube-system/<node name>`), accepted only while
that object belongs to the Node's current UID and holds exactly one network
container, of type `overlay`, whose `nodeIP` is the Node's `InternalIP`, and the
`/24` lies in the pod CIDR and overlaps no other node's range. The agent
pins each node's range, region, VM ID and IP in memory when it accepts them; a
later change removes the node as a peer. A node is a multi-region node when it is
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
UDP 4789 only from peers' node IPs and sets TCP MSS 1360 on SYNs and SYN-ACKs
into the tunnel. Pods keep the MTU that `cilium-cni` gives them, on multi-region
nodes as on home nodes, so their packets into the tunnel can be longer than
1400 bytes: TCP gets that MSS, and a longer packet of another protocol is
fragmented at the bridge or, with the don't-fragment flag, dropped with an ICMP
error that tells the sender the path MTU. It does not start when the pod CIDR
overlaps `169.254.0.0/16`, or when
the transit prefix is not inside `169.254.0.0/16`, has host bits, is too small
for the pod CIDR or contains `169.254.169.254`. It programs the tunnel when it
starts, once a minute, when what it must program changes and after a failed
attempt, and each time it first checks the node: it changes nothing while
another VXLAN device uses UDP 4789, a device with one of its names is not its
own, or a host address that it did not assign lies in the transit prefix; what
it programmed before stays. If it cannot finish creating one of its devices,
it deletes the device again. When it is stopped, the running pass ends early:
its requests to the API server, to instance metadata and to the Cilium agent
are cancelled and it leaves the taint alone, but programming of the tunnel or
of the CNI file that is in progress completes first. A device stays unmarked
when its deletion fails as well, or when the agent is killed or crashes
between creating the device and marking it as its own; the agent then refuses
the node until the device is deleted or the node restarts. A home node's peers
are the multi-region nodes, and its CNI configuration stays as AKS wrote it. A
multi-region node's peers are all other nodes, and there the agent also:

- writes a CNI configuration that sorts before every other file in
  `/etc/cni/net.d`: the `cilium-cni` plugin with `host-local` IPAM over the
  node's `/24` (addresses `.2` to `.254`, gateway `.1`);
- masquerades pod traffic that leaves through the node's own interface;
- drops forwarded traffic to `169.254.169.254`, so that pods cannot reach
  instance metadata.

A multi-region node offers 253 pod addresses, so a NodeClass that sets `maxPods`
above 253 is not supported on multi-region nodes. The plugins `cilium-cni` and
`host-local` must be executable files in `/opt/cni/bin`; while one is missing,
the agent keeps a multi-region node not ready. The configuration leaves the MTU of
the pods to `cilium-cni`: a lower MTU on a pod's interface would not lower what
the pod sends, because `cilium-cni` gives the pod's routes Cilium's MTU, but
the veth would silently drop longer packets on their way into the pod, such as
full-size replies from outside the cluster.

The agent writes that CNI file once the node's Cilium agent is healthy, and
only while `/etc/cni/net.d` holds no other CNI configuration: pods started with
another configuration could hold addresses that `host-local` does not know. On
AKS, Azure CNS writes the CNI configuration of the Cilium dataplane only after
the Azure host reports the node's network container as programmed, and hands
out no pod address until then. Once the agent's file is in place, a later CNI
configuration does not matter, and an earlier one makes the node not ready.
The agent refuses a node whose file holds other bytes, with one exception: it
replaces the file that the previous agent version wrote for the node's `/24`,
which also chained the `tuning` plugin for MTU 1400, unless another CNI
configuration sorts before it. It writes its file under a temporary name that
the container runtime ignores and renames it over the old one, so that the
runtime reads either file whole. Both files name the same network and range,
so `host-local` keeps the addresses that it handed out. Pods that already run
on the node keep MTU 1400 until they are recreated, so longer packets on their
way into such a pod are still dropped. A file of an older version or of
another range keeps the node not ready and tainted until the node is replaced.

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
programmed, and on a multi-region node also the node's Cilium agent is healthy
(`http://127.0.0.1:9879/healthz` with the header `brief: true`) and the CNI file
is in place. It puts the taint back only on a node whose NodeClaim, of this
controller's node class group, carries the
`karpenter.azure.com/region-placement` annotation, when any of that fails
later, only on the Node with the UID it checked and unless that Node is being
deleted. One failed pass is enough: while the node's Cilium agent restarts, for
example, a multi-region node carries the taint until a pass succeeds again, which
the agent retries every 5 seconds, and the pods that run on it keep running.
The agent's readiness probe reports the same conditions, apart from the
registration. A multi-region node that never becomes ready stays tainted until its
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
Security level. It holds no Azure credentials. It may get, list, watch and
patch Nodes, list and watch NodeClaims, and in `kube-system` list and watch
`nodenetworkconfigs.acn.azure.com` and get the `cilium-config` ConfigMap. A
ValidatingAdmissionPolicy, which the chart installs with the agent unless
`podNetworkAgent.admissionPolicy.enabled=false`, lets it patch only its own
Node and only the startup taint. It recognizes the agent's Node by the node name
in the agent's service account token, which Kubernetes adds from version 1.30.

## FIPS node images

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
