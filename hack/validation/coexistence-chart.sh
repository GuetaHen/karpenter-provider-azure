#!/usr/bin/env bash
set -euo pipefail

# Validates that both charts install coexistence mode by values alone:
# - the CRD chart renders only the multiregion.io AKSNodeClass CRD in coexistence
#   mode, and every other CRD otherwise;
# - the controller chart refuses coexistence mode with the webhooks enabled, sets
#   COEXISTENCE_MODE, and grants access to multiregion.io AKSNodeClasses plus get on
#   karpenter.azure.com AKSNodeClasses instead of access to the latter; and
# - the controller may get the maintenance-window ConfigMap in the namespace that
#   SYSTEM_NAMESPACE names.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

crd_names() {
    yq eval-all -o=json 'select(.kind == "CustomResourceDefinition") | .metadata.name' "$1" | jq -r . | LC_ALL=C sort | paste -sd, -
}
env_value() {
    yq eval-all "select(.kind == \"Deployment\") | .spec.template.spec.containers[0].env[] | select(.name == \"$2\") | .value" "$1"
}
nodeclass_rules() {
    yq eval-all -o=json 'select(.kind == "ClusterRole" and .metadata.name == "karpenter") | .rules' "$1"
}
# Succeeds when a RoleBinding in namespace $2 binds a Role there that may get the ConfigMap.
maintenance_window_access() {
    yq eval-all -o=json 'select(.kind == "Role" or .kind == "RoleBinding")' "$1" | jq -se --arg ns "$2" '
        map(select(.metadata.namespace == $ns)) as $objects
        | [$objects[] | select(.kind == "Role" and any(.rules[]; .resources == ["configmaps"] and .resourceNames == ["upcoming-maintenance-window"] and .verbs == ["get"])) | .metadata.name] as $roles
        | any($objects[]; .kind == "RoleBinding" and (.roleRef.name as $name | any($roles[]; . == $name)))' >/dev/null
}

helm template karpenter-crd charts/karpenter-crd >"$tmp_dir/crds.yaml"
helm template karpenter-crd charts/karpenter-crd --set coexistence.enabled=true >"$tmp_dir/coexistence-crds.yaml"
[[ "$(crd_names "$tmp_dir/crds.yaml")" == "aksnodeclasses.karpenter.azure.com,capacitybuffers.autoscaling.x-k8s.io,nodeclaims.karpenter.sh,nodeoverlays.karpenter.sh,nodepools.karpenter.sh" ]] || {
    echo "expected the CRD chart to render every CRD except the multiregion.io one by default"
    exit 1
}
[[ "$(crd_names "$tmp_dir/coexistence-crds.yaml")" == "aksnodeclasses.multiregion.io" ]] || {
    echo "expected the CRD chart to render only the multiregion.io AKSNodeClass CRD in coexistence mode"
    exit 1
}

if helm template karpenter charts/karpenter --namespace karpenter --set coexistence.enabled=true \
    >/dev/null 2>"$tmp_dir/refused.txt" || ! grep -q 'coexistence.enabled requires webhook.enabled=false' "$tmp_dir/refused.txt"; then
    echo "expected the controller chart to refuse coexistence mode with the webhooks enabled"
    exit 1
fi
helm template karpenter charts/karpenter --namespace karpenter >"$tmp_dir/default.yaml"
helm template karpenter charts/karpenter --namespace karpenter --set coexistence.enabled=true \
    --set webhook.enabled=false --set settings.systemNamespace=kube-system >"$tmp_dir/coexistence.yaml"

[[ -z "$(env_value "$tmp_dir/default.yaml" COEXISTENCE_MODE)" &&
    "$(env_value "$tmp_dir/coexistence.yaml" COEXISTENCE_MODE)" == "true" &&
    "$(env_value "$tmp_dir/coexistence.yaml" SYSTEM_NAMESPACE)" == "kube-system" ]] || {
    echo "expected COEXISTENCE_MODE=true only in coexistence mode, and SYSTEM_NAMESPACE from settings.systemNamespace"
    exit 1
}
jq -e '. == [
    {"apiGroups": ["karpenter.azure.com"], "resources": ["aksnodeclasses"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["karpenter.azure.com"], "resources": ["aksnodeclasses", "aksnodeclasses/status"], "verbs": ["patch", "update"]}
]' <<<"$(nodeclass_rules "$tmp_dir/default.yaml")" >/dev/null
jq -e '. == [
    {"apiGroups": ["multiregion.io"], "resources": ["aksnodeclasses"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["karpenter.azure.com"], "resources": ["aksnodeclasses"], "verbs": ["get"]},
    {"apiGroups": ["multiregion.io"], "resources": ["aksnodeclasses", "aksnodeclasses/status"], "verbs": ["patch", "update"]}
]' <<<"$(nodeclass_rules "$tmp_dir/coexistence.yaml")" >/dev/null
maintenance_window_access "$tmp_dir/default.yaml" karpenter || {
    echo "expected get on the maintenance-window ConfigMap in the release namespace by default"
    exit 1
}
maintenance_window_access "$tmp_dir/coexistence.yaml" kube-system || {
    echo "expected get on the maintenance-window ConfigMap in settings.systemNamespace"
    exit 1
}
