#!/usr/bin/env bash
set -euo pipefail

# Validates the pod network agent of the controller chart:
# - by default the chart renders no object of the agent and passes no startup taint key to the
#   controller;
# - with the agent on, it requires the image, the pod CIDR and the node resource group ID, and passes
#   the startup taint key to the controller;
# - the DaemonSet runs the agent on every Linux node with host networking and the node's DNS settings,
#   tolerates every taint, gives the agent a minute to finish a pass when it stops, replaces its pods
#   one node at a time without surge, runs the agent as root with only NET_ADMIN and a read-only root
#   file system, mounts the CNI directories, runs the entrypoint of the image unless
#   podNetworkAgent.command is set, and points the agent at podNetworkAgent.apiServer when that is set;
# - the agent may read Nodes and NodeClaims, patch Nodes, and read NodeNetworkConfigs and
#   cilium-config in kube-system; and
# - the admission policy is the policy file with the agent's user and startup taint key, unless
#   podNetworkAgent.admissionPolicy.enabled=false.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

name=karpenter-pod-network-agent
key=karpenter.azure.com/regional-pod-network-not-ready
group_id=/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/cluster-nodes
digest=sha256:0000000000000000000000000000000000000000000000000000000000000000
agent=(--set podNetworkAgent.enabled=true --set podNetworkAgent.image.repository=example.com/pod-network-agent
    --set podNetworkAgent.image.tag=v0.0.0 --set podNetworkAgent.podCIDR=10.244.0.0/16
    --set podNetworkAgent.nodeResourceGroupID="$group_id")

fail() {
    echo "$1"
    exit 1
}
render() {
    helm template karpenter charts/karpenter --namespace karpenter "$@"
}
# Lists the kinds of the agent's objects.
agent_kinds() {
    yq eval-all -o=json "select(.metadata.name == \"$name\") | .kind" "$1" | jq -r . | LC_ALL=C sort | paste -sd, -
}
controller_env() {
    yq eval-all "select(.kind == \"Deployment\") | .spec.template.spec.containers[0].env[] | select(.name == \"$2\") | .value" "$1"
}
# Prints the agent's object of a kind as JSON.
agent_object() {
    yq eval-all -o=json "select(.kind == \"$2\" and .metadata.name == \"$name\")" "$1"
}
# Fails unless rendering with the arguments fails with the message.
expect_refused() {
    local message=$1
    shift
    if render "$@" >/dev/null 2>"$tmp_dir/refused.txt" || ! grep -q "$message" "$tmp_dir/refused.txt"; then
        fail "expected the chart to refuse with: $message"
    fi
}

render >"$tmp_dir/default.yaml"
[[ -z "$(agent_kinds "$tmp_dir/default.yaml")" && -z "$(controller_env "$tmp_dir/default.yaml" MULTI_REGION_POD_NETWORK_STARTUP_TAINT)" ]] ||
    fail "expected no object of the pod network agent and no startup taint key for the controller by default"

for value in image.repository podCIDR nodeResourceGroupID; do
    expect_refused "podNetworkAgent.$value is required" "${agent[@]}" --set "podNetworkAgent.$value="
done
expect_refused "podNetworkAgent.image needs a tag or a digest" "${agent[@]}" --set podNetworkAgent.image.tag=

render "${agent[@]}" >"$tmp_dir/agent.yaml"
[[ "$(agent_kinds "$tmp_dir/agent.yaml")" == "ClusterRole,ClusterRoleBinding,DaemonSet,Role,RoleBinding,ServiceAccount,ValidatingAdmissionPolicy,ValidatingAdmissionPolicyBinding" ]] ||
    fail "expected the objects of the pod network agent"
[[ "$(controller_env "$tmp_dir/agent.yaml" MULTI_REGION_POD_NETWORK_STARTUP_TAINT)" == "$key" ]] ||
    fail "expected the controller to get the startup taint key"

agent_object "$tmp_dir/agent.yaml" DaemonSet | jq -e --arg key "$key" --arg group "$group_id" --arg name "$name" '
    .spec.updateStrategy == {"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": 1}}
    and .spec.selector.matchLabels == .spec.template.metadata.labels
    and .spec.template.metadata.labels["app.kubernetes.io/name"] == $name
    and (.spec.template.spec | .hostNetwork == true and .dnsPolicy == "Default" and .terminationGracePeriodSeconds == 60
        and .priorityClassName == "system-node-critical" and .serviceAccountName == $name
        and .tolerations == [{"operator": "Exists"}] and .nodeSelector == {"kubernetes.io/os": "linux"}
        and .securityContext == {"seccompProfile": {"type": "RuntimeDefault"}}
        and .volumes == [
            {"name": "cni-conf", "hostPath": {"path": "/etc/cni/net.d", "type": "DirectoryOrCreate"}},
            {"name": "cni-bin", "hostPath": {"path": "/opt/cni/bin", "type": "Directory"}}]
        and (.containers | length) == 1)
    and (.spec.template.spec.containers[0]
        | .image == "example.com/pod-network-agent:v0.0.0" and (has("command") | not)
        and .securityContext == {"runAsUser": 0, "runAsNonRoot": false, "privileged": false,
            "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
            "capabilities": {"drop": ["ALL"], "add": ["NET_ADMIN"]}}
        and ([.env[] | {(.name): (.value // .valueFrom)}] | add) == {
            "NODE_NAME": {"fieldRef": {"fieldPath": "spec.nodeName"}}, "POD_CIDR": "10.244.0.0/16",
            "NODE_RESOURCE_GROUP_ID": $group, "NODE_CLASS_GROUP": "karpenter.azure.com",
            "TRANSIT_PREFIX": "169.254.64.0/18", "STARTUP_TAINT_KEY": $key, "PROBE_ADDRESS": "127.0.0.1:9887"}
        and .readinessProbe.httpGet == {"host": "127.0.0.1", "path": "/readyz", "port": 9887}
        and .livenessProbe.httpGet == {"host": "127.0.0.1", "path": "/healthz", "port": 9887}
        and .volumeMounts == [{"name": "cni-conf", "mountPath": "/etc/cni/net.d"},
            {"name": "cni-bin", "mountPath": "/opt/cni/bin", "readOnly": true}])
' >/dev/null || fail "expected the DaemonSet of the pod network agent as docs/multi-region.md describes it"
# The controller's Service, PodDisruptionBudget and anti-affinity must not select the agent's pods.
[[ "$(yq eval-all 'select(.kind == "Deployment") | .spec.selector.matchLabels["app.kubernetes.io/name"]' "$tmp_dir/agent.yaml")" == karpenter ]] ||
    fail "expected the controller to keep its own selector"

agent_object "$tmp_dir/agent.yaml" ClusterRole | jq -e '.rules == [
    {"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list", "watch", "patch"]},
    {"apiGroups": ["karpenter.sh"], "resources": ["nodeclaims"], "verbs": ["get", "list", "watch"]}]' >/dev/null ||
    fail "expected the agent to read Nodes and NodeClaims and to patch Nodes"
agent_object "$tmp_dir/agent.yaml" Role | jq -e '.metadata.namespace == "kube-system" and .rules == [
    {"apiGroups": ["acn.azure.com"], "resources": ["nodenetworkconfigs"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["cilium-config"], "verbs": ["get"]}]' >/dev/null ||
    fail "expected the agent to read NodeNetworkConfigs and cilium-config in kube-system"
for kind in ClusterRoleBinding RoleBinding; do
    agent_object "$tmp_dir/agent.yaml" "$kind" | jq -e --arg name "$name" '.roleRef.name == $name
        and .subjects == [{"kind": "ServiceAccount", "name": $name, "namespace": "karpenter"}]' >/dev/null ||
        fail "expected the $kind to give the role of the agent to its ServiceAccount"
done

sed -e "s|__AGENT_USERNAME__|system:serviceaccount:karpenter:$name|g" -e "s|__STARTUP_TAINT_KEY__|$key|g" \
    charts/karpenter/files/pod-network-agent-policy.yaml | yq -o=json . >"$tmp_dir/policy.json"
agent_object "$tmp_dir/agent.yaml" ValidatingAdmissionPolicy | jq -e --slurpfile spec "$tmp_dir/policy.json" '.spec == $spec[0]' >/dev/null ||
    fail "expected the admission policy of the policy file with the agent's user and startup taint key"
agent_object "$tmp_dir/agent.yaml" ValidatingAdmissionPolicyBinding | jq -e --arg name "$name" '.spec == {"policyName": $name, "validationActions": ["Deny"]}' >/dev/null ||
    fail "expected the binding of the admission policy"
render "${agent[@]}" --set podNetworkAgent.admissionPolicy.enabled=false >"$tmp_dir/no-policy.yaml"
[[ "$(agent_kinds "$tmp_dir/no-policy.yaml")" == "ClusterRole,ClusterRoleBinding,DaemonSet,Role,RoleBinding,ServiceAccount" ]] ||
    fail "expected no admission policy with podNetworkAgent.admissionPolicy.enabled=false"

render "${agent[@]}" --set podNetworkAgent.image.digest="$digest" --set 'podNetworkAgent.command={/usr/local/bin/pod-network-agent}' \
    --set podNetworkAgent.apiServer.host=cluster.example.com --set coexistence.enabled=true --set webhook.enabled=false \
    >"$tmp_dir/options.yaml"
agent_object "$tmp_dir/options.yaml" DaemonSet | jq -e --arg image "example.com/pod-network-agent:v0.0.0@$digest" '
    .spec.template.spec.containers[0] | .image == $image and .command == ["/usr/local/bin/pod-network-agent"]
    and ([.env[] | {(.name): .value}] | add | .NODE_CLASS_GROUP == "multiregion.io"
        and .KUBERNETES_SERVICE_HOST == "cluster.example.com" and .KUBERNETES_SERVICE_PORT == "443")' >/dev/null ||
    fail "expected the image digest, the command, the API server and the node class group of coexistence mode"
render "${agent[@]}" --set podNetworkAgent.image.tag= --set podNetworkAgent.image.digest="$digest" >"$tmp_dir/digest.yaml"
[[ "$(agent_object "$tmp_dir/digest.yaml" DaemonSet | jq -r '.spec.template.spec.containers[0].image')" == "example.com/pod-network-agent@$digest" ]] ||
    fail "expected an image by digest alone"
