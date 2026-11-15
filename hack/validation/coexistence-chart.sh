#!/usr/bin/env bash
set -euo pipefail

# Validates that the charts install coexistence mode by values and --skip-crds:
# - the karpenter-coexistence-crd chart renders only the multiregion.io AKSNodeClass
#   CRD, which Helm keeps when its release is uninstalled, and the karpenter-crd
#   chart renders every other CRD;
# - the controller chart refuses coexistence mode with the webhooks enabled, sets
#   COEXISTENCE_MODE, grants access to multiregion.io AKSNodeClasses plus get on
#   karpenter.azure.com AKSNodeClasses instead of access to the latter, and grants
#   no write on NodeOverlay status, which only the default render keeps; and
# - the controller may get the maintenance-window ConfigMap in the namespace that
#   SYSTEM_NAMESPACE names, while the default render adds no object for that.

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
# Counts the rules of ClusterRoles bound to the controller that let it write NodeOverlay status.
overlay_status_writers() {
    yq eval-all -o=json 'select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding")' "$1" | jq -s '
        [.[] | select(.kind == "ClusterRoleBinding" and any((.subjects // [])[]; .kind == "ServiceAccount" and .name == "karpenter")) | .roleRef.name] as $bound
        | [.[] | select(.kind == "ClusterRole" and (.metadata.name as $name | any($bound[]; . == $name))) | (.rules // [])[]
            | select(any((.apiGroups // [])[]; . == "karpenter.sh" or . == "*")
                and any((.resources // [])[]; . == "nodeoverlays/status" or . == "*")
                and any((.verbs // [])[]; . == "update" or . == "patch" or . == "*"))] | length'
}
# Succeeds when a RoleBinding in namespace $2 binds a Role there that may get the ConfigMap.
maintenance_window_access() {
    yq eval-all -o=json 'select(.kind == "Role" or .kind == "RoleBinding")' "$1" | jq -se --arg ns "$2" '
        map(select(.metadata.namespace == $ns)) as $objects
        | [$objects[] | select(.kind == "Role" and any(.rules[]; .resources == ["configmaps"] and .resourceNames == ["upcoming-maintenance-window"] and .verbs == ["get"])) | .metadata.name] as $roles
        | any($objects[]; .kind == "RoleBinding" and (.roleRef.name as $name | any($roles[]; . == $name)))' >/dev/null
}

helm lint charts/karpenter-coexistence-crd >"$tmp_dir/lint.txt" || {
    cat "$tmp_dir/lint.txt"
    exit 1
}
helm template karpenter-crd charts/karpenter-crd >"$tmp_dir/crds.yaml"
helm template karpenter-coexistence-crd charts/karpenter-coexistence-crd >"$tmp_dir/coexistence-crds.yaml"
[[ "$(crd_names "$tmp_dir/crds.yaml")" == "aksnodeclasses.karpenter.azure.com,capacitybuffers.autoscaling.x-k8s.io,nodeclaims.karpenter.sh,nodeoverlays.karpenter.sh,nodepools.karpenter.sh" ]] || {
    echo "expected the karpenter-crd chart to render every CRD except the multiregion.io one"
    exit 1
}
[[ "$(crd_names "$tmp_dir/coexistence-crds.yaml")" == "aksnodeclasses.multiregion.io" &&
    "$(yq eval-all 'select(.kind == "CustomResourceDefinition") | .metadata.annotations["helm.sh/resource-policy"]' "$tmp_dir/coexistence-crds.yaml")" == "keep" ]] || {
    echo "expected the karpenter-coexistence-crd chart to render only the multiregion.io AKSNodeClass CRD, which Helm keeps on uninstall"
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
[[ "$(overlay_status_writers "$tmp_dir/default.yaml")" == 1 && "$(overlay_status_writers "$tmp_dir/coexistence.yaml")" == 0 ]] || {
    echo "expected update and patch on nodeoverlays/status by default only, because coexistence mode never writes NodeOverlay status"
    exit 1
}
# With the webhooks on, the controller's Role already reads ConfigMaps in the release namespace.
[[ "$(yq eval-all -o=json 'select(.metadata.name == "karpenter-maintenance-window") | .kind' "$tmp_dir/default.yaml" | jq -s length)" == 0 ]] || {
    echo "expected no maintenance-window Role or RoleBinding in the default render"
    exit 1
}
helm template karpenter charts/karpenter --namespace karpenter --set webhook.enabled=false >"$tmp_dir/no-webhook.yaml"
maintenance_window_access "$tmp_dir/no-webhook.yaml" karpenter || {
    echo "expected get on the maintenance-window ConfigMap in the release namespace without the webhooks"
    exit 1
}
maintenance_window_access "$tmp_dir/coexistence.yaml" kube-system || {
    echo "expected get on the maintenance-window ConfigMap in settings.systemNamespace"
    exit 1
}
