#!/usr/bin/env bash
# check-helm-rbac.sh — guard against a Helm chart's RBAC drifting behind the
# kubebuilder markers of the operator that chart ships (#379, the RBAC analogue
# of the #367 CRD sync guard).
#
# Each operator's ClusterRole is hand-maintained in its own chart:
#
#   internal/controller/**          -> charts/llmkube  (inference operator)
#   internal/foreman/controller/**  -> charts/foreman   (foreman operator)
#
# A marker that lands in the source but not in that operator's chart silently
# breaks the in-cluster binary: controller-runtime's cache cannot list the type,
# the reflector retries forever, and the feature is dead with only a "Failed to
# watch" line to show for it (#376 Endpoints, #1593 batch/jobs).
#
# WHY THIS READS MARKERS FROM SOURCE RATHER THAN config/rbac/role.yaml
#
# role.yaml is generated with paths="./...", so it is the UNION of both
# operators' markers with no record of which operator needs which rule. An
# earlier version of this check compared role.yaml against the union of BOTH
# charts, which made the two operators cover for each other: charts/foreman
# grants batch/jobs and pods/log to the foreman AGENT ServiceAccount for
# run_gate_job, and that satisfied the inference operator's identically-shaped
# markers. The check reported "covers all 264 rules" while the llmkube
# controller-manager had neither grant and #1593 was live in a release.
#
# Attributing each marker to the tree it came from is what closes that hole.
# Extra chart rules are fine: each chart must be a SUPERSET of its operator's
# markers, not an exact match.
#
# THE NAMESPACE-SCOPED RENDER (#1600)
#
# With controllerManager.watchNamespaces set, charts/llmkube moves the rules on
# namespaced resources out of the ClusterRole into a manager Role in each
# watched namespace. So the llmkube chart is checked twice: the default render
# as above, and a render with two watched namespaces, where
#
#   - in every watched namespace, the ClusterRole plus that namespace's
#     manager Role must cover every marker, and
#   - a marker on a cluster-scoped resource must be covered by the ClusterRole
#     alone. A Role cannot grant one, so a cluster-scoped rule filed under the
#     namespaced half would read as granted and 403 in a scoped install.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

command -v helm >/dev/null 2>&1 || { echo "❌ helm is required"; exit 2; }
python3 -c 'import yaml' >/dev/null 2>&1 || { echo "❌ python3 with PyYAML is required (pip install pyyaml)"; exit 2; }

render() {
  local chart="$1"
  shift
  helm template "$chart" --set rbac.create=true --set crds.install=false "$@" 2>/dev/null || {
    echo "❌ helm template failed for $chart $*" >&2
    exit 2
  }
}

# Two namespaces, so a Role rendered for only the first one is caught.
SCOPED_NAMESPACES="rbac-check-a,rbac-check-b"

# Render each chart to its own temp file. A temp FILE, not an env var: the full
# render can exceed the OS env+argv limit (E2BIG) once a chart emits base64
# cert blobs.
LLMKUBE_YAML="$(mktemp)"
LLMKUBE_SCOPED_YAML="$(mktemp)"
FOREMAN_YAML="$(mktemp)"
trap 'rm -f "$LLMKUBE_YAML" "$LLMKUBE_SCOPED_YAML" "$FOREMAN_YAML"' EXIT
render "$REPO_ROOT/charts/llmkube" > "$LLMKUBE_YAML"
render "$REPO_ROOT/charts/llmkube" \
  --set "controllerManager.watchNamespaces={$SCOPED_NAMESPACES}" > "$LLMKUBE_SCOPED_YAML"
render "$REPO_ROOT/charts/foreman" > "$FOREMAN_YAML"

REPO_ROOT="$REPO_ROOT" LLMKUBE_YAML="$LLMKUBE_YAML" LLMKUBE_SCOPED_YAML="$LLMKUBE_SCOPED_YAML" \
  SCOPED_NAMESPACES="$SCOPED_NAMESPACES" FOREMAN_YAML="$FOREMAN_YAML" python3 - <<'PY'
import os, re, sys, yaml
from pathlib import Path

REPO = Path(os.environ["REPO_ROOT"])

OPERATORS = [
    {
        "label": "inference operator",
        "src": "internal/controller",
        "chart": "charts/llmkube",
        "yaml": os.environ["LLMKUBE_YAML"],
        "scoped_yaml": os.environ["LLMKUBE_SCOPED_YAML"],
        "fix": "charts/llmkube/templates/_manager-rules.tpl",
    },
    {
        "label": "foreman operator",
        "src": "internal/foreman/controller",
        "chart": "charts/foreman",
        "yaml": os.environ["FOREMAN_YAML"],
        "fix": "charts/foreman/templates/operator-rbac.yaml",
    },
]

MARKER = re.compile(r'^\s*//\s*\+kubebuilder:rbac:(.+?)\s*$')


def marker_triples(src_dir):
    """(group, resource, verb) required by the operator rooted at src_dir.

    Only comment-form markers in non-test files. _test.go is excluded because
    pkg/foreman/agent/rbac_gate_test.go carries real marker syntax as fixture
    data for the gate that lints markers, and those are not grants anyone needs.
    """
    out = set()
    for path in sorted((REPO / src_dir).rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for line in path.read_text().splitlines():
            m = MARKER.match(line)
            if not m:
                continue
            fields = {}
            for part in m.group(1).split(","):
                if "=" not in part:
                    continue
                k, v = part.split("=", 1)
                fields[k.strip()] = v.strip()
            # controller-gen normalises the core group: both groups=core and
            # groups="" render as apiGroups: [""].
            groups = [("" if g.strip('"') == "core" else g.strip('"'))
                      for g in fields.get("groups", "").split(";")]
            for g in groups:
                for r in fields.get("resources", "").split(";"):
                    for v in fields.get("verbs", "").split(";"):
                        if r and v:
                            out.add((g, r, v))
    return out


def chart_triples(path):
    """(group, resource, verb) the chart's ClusterRoles grant.

    ClusterRole only, deliberately. A namespaced Role is a grant in one
    namespace, so it cannot cover a marker for a cluster-wide watch: a
    ControllerManager whose cache cannot list a type cluster-wide is the
    failure this check exists to catch (#376, #1593). The llmkube chart's
    leader-election Role grants configmaps, events and coordination leases in
    the release namespace. Accepting it here let a lease marker read as covered
    while the operator's ClusterRole had none, so every pooled ModelRouter got
    a 403 on its Lease (found on the #1477 branch, never on main).
    """
    out = set()
    for doc in yaml.safe_load_all(Path(path).read_text()):
        if not doc or doc.get("kind") != "ClusterRole":
            continue
        for rule in (doc.get("rules") or []):
            for g in (rule.get("apiGroups") or []):
                for r in (rule.get("resources") or []):
                    for v in (rule.get("verbs") or []):
                        out.add((g, r, v))
    return out


# Built-in resources that are cluster-scoped. Deliberately wider than what the
# operator reads today, so a future marker on any of them is recognised
# without anyone remembering to extend this list. Cluster-scoped CRDs are read
# from config/crd/bases instead (cluster_scoped_crds).
CLUSTER_SCOPED_BUILTINS = {
    ("", "namespaces"), ("", "nodes"), ("", "persistentvolumes"),
    ("scheduling.k8s.io", "priorityclasses"),
    ("storage.k8s.io", "storageclasses"), ("storage.k8s.io", "csidrivers"),
    ("storage.k8s.io", "csinodes"), ("storage.k8s.io", "volumeattachments"),
    ("node.k8s.io", "runtimeclasses"),
    ("rbac.authorization.k8s.io", "clusterroles"),
    ("rbac.authorization.k8s.io", "clusterrolebindings"),
    ("apiextensions.k8s.io", "customresourcedefinitions"),
    ("apiregistration.k8s.io", "apiservices"),
    ("admissionregistration.k8s.io", "validatingwebhookconfigurations"),
    ("admissionregistration.k8s.io", "mutatingwebhookconfigurations"),
    ("admissionregistration.k8s.io", "validatingadmissionpolicies"),
    ("admissionregistration.k8s.io", "validatingadmissionpolicybindings"),
    ("authentication.k8s.io", "tokenreviews"),
    ("authorization.k8s.io", "subjectaccessreviews"),
    ("certificates.k8s.io", "certificatesigningrequests"),
    ("networking.k8s.io", "ingressclasses"),
    ("gateway.networking.k8s.io", "gatewayclasses"),
    ("resource.k8s.io", "deviceclasses"), ("resource.k8s.io", "resourceslices"),
}


def cluster_scoped_crds():
    """(group, plural) of every CRD in config/crd/bases with scope: Cluster."""
    out = set()
    for path in sorted((REPO / "config/crd/bases").glob("*.yaml")):
        for doc in yaml.safe_load_all(path.read_text()):
            spec = (doc or {}).get("spec") or {}
            if spec.get("scope") == "Cluster":
                out.add((spec["group"], spec["names"]["plural"]))
    return out


def is_cluster_scoped(triple, cluster_scoped):
    g, r, _ = triple
    # A subresource (nodes/status, federatedclusters/status) has its parent's
    # scope.
    return (g, r.split("/", 1)[0]) in cluster_scoped


def manager_role_triples(path, namespace):
    """(group, resource, verb) the manager Role in namespace grants.

    Only the Role named *-manager-role. The leader-election Role lives in the
    release namespace too and grants configmaps, events and leases there;
    counting it would let a missing manager rule read as covered in that one
    namespace, the same trap chart_triples describes for the ClusterRole.
    """
    out = set()
    for doc in yaml.safe_load_all(Path(path).read_text()):
        if not doc or doc.get("kind") != "Role":
            continue
        meta = doc.get("metadata") or {}
        if meta.get("namespace") != namespace or not meta.get("name", "").endswith("-manager-role"):
            continue
        for rule in (doc.get("rules") or []):
            for g in (rule.get("apiGroups") or []):
                for r in (rule.get("resources") or []):
                    for v in (rule.get("verbs") or []):
                        out.add((g, r, v))
    return out


def covered(triple, granted):
    g, r, v = triple
    if triple in granted:
        return True
    # Honour wildcards on the chart side so a legitimate broad grant does not
    # read as a gap. None exist today; this keeps the check from false-failing
    # if one is ever added.
    for cg, cr, cv in granted:
        if cg in (g, "*") and cr in (r, "*") and cv in (v, "*"):
            return True
    return False


failed = False
total_required = 0
for op in OPERATORS:
    required = marker_triples(op["src"])
    granted = chart_triples(op["yaml"])
    total_required += len(required)
    missing = sorted(t for t in required if not covered(t, granted))
    if missing:
        failed = True
        print("❌ %s: %s/ has markers that %s does not grant:"
              % (op["label"], op["src"], op["chart"]))
        for g, r, v in missing:
            print("     apiGroup=%-24s resource=%-28s verb=%s"
                  % (repr(g), r, v))
        print("   Add them to %s" % op["fix"])
        print()

    if "scoped_yaml" not in op:
        continue
    cluster_scoped = CLUSTER_SCOPED_BUILTINS | cluster_scoped_crds()
    cluster_granted = chart_triples(op["scoped_yaml"])
    not_in_cluster_role = sorted(
        t for t in required
        if is_cluster_scoped(t, cluster_scoped) and not covered(t, cluster_granted))
    if not_in_cluster_role:
        failed = True
        print("❌ %s: with controllerManager.watchNamespaces set, %s leaves these"
              % (op["label"], op["chart"]))
        print("   cluster-scoped rules out of the ClusterRole. A Role cannot grant them:")
        for g, r, v in not_in_cluster_role:
            print("     apiGroup=%-24s resource=%-28s verb=%s"
                  % (repr(g), r, v))
        print("   Move them to llmkube.manager.clusterRules in %s" % op["fix"])
        print()
    for namespace in os.environ["SCOPED_NAMESPACES"].split(","):
        granted = cluster_granted | manager_role_triples(op["scoped_yaml"], namespace)
        missing = sorted(t for t in required
                         if not is_cluster_scoped(t, cluster_scoped) and not covered(t, granted))
        if missing:
            failed = True
            print("❌ %s: with controllerManager.watchNamespaces set, namespace %s is"
                  % (op["label"], namespace))
            print("   not granted these markers by the ClusterRole plus its manager Role:")
            for g, r, v in missing:
                print("     apiGroup=%-24s resource=%-28s verb=%s"
                      % (repr(g), r, v))
            print("   Add them to llmkube.manager.namespacedRules in %s" % op["fix"])
            print()

if failed:
    print("Each chart must be a superset of ITS OWN operator's markers.")
    print("A grant in the other chart does not count: it belongs to a")
    print("different ServiceAccount and will not be bound to this operator.")
    sys.exit(1)

print("✅ Each chart covers its own operator's markers (%d rules across %d operators),"
      % (total_required, len(OPERATORS)))
print("   and charts/llmkube still does with controllerManager.watchNamespaces set.")
PY
