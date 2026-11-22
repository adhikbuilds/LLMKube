---
title: Namespace-scoped install
description: Run the LLMKube operator against a fixed list of namespaces with per-namespace Roles instead of a cluster-wide ClusterRole.
---

# Namespace-scoped install

By default the operator watches every namespace, and its ClusterRole grants
read and write on Deployments, Services, Pods, PVCs, Secrets and ConfigMaps
across the whole cluster. If LLMKube only ever serves one or two namespaces,
you can limit it to those:

```bash
kubectl create namespace team-a
helm install llmkube llmkube/llmkube \
  -n llmkube-system --create-namespace \
  --set 'controllerManager.watchNamespaces={team-a}'
```

The operator's own namespace (`llmkube-system` here) is always part of the
list, so this install watches `llmkube-system` and `team-a`. To watch only the
operator's namespace, list it: `--set 'controllerManager.watchNamespaces={llmkube-system}'`.

The namespaces must exist before the install. The chart creates a Role in each
one and does not create the namespace.

## What changes

| | Default | With `watchNamespaces` |
| --- | --- | --- |
| Operator cache | every namespace | the listed namespaces |
| Rules on namespaced resources | one ClusterRole | a Role and RoleBinding in each listed namespace |
| ClusterRole | everything | cluster-scoped resources only |
| Controller flag | none | `--watch-namespaces=<list>` |

The chart passes the same list to the controller's `--watch-namespaces` flag
and to the RBAC templates, so the cache and the grants cannot disagree.

## What stays cluster-wide

Some resources the operator reads are cluster-scoped, and no Role can grant
them. A namespace-scoped install still has a ClusterRole, with these rules
only:

| Resource | Verbs |
| --- | --- |
| `nodes`, `namespaces` | get, list, watch |
| `persistentvolumes` | get, list, watch |
| `priorityclasses` | get, list, watch |
| `federatedclusters` | get, list, watch |
| `federatedclusters/status` | get, patch, update |

So the result is a much smaller ClusterRole, not an install with none.

## Objects in other namespaces

The operator does not see a namespace outside the list. A Model or
InferenceService created there is not reconciled: it gets no status, no
conditions and no events, because the operator has no watch on it and no
permission to write to it. The controller logs the namespaces it watches once
at startup:

```
watching the listed namespaces only; objects in other namespaces are not reconciled  {"namespaces": ["llmkube-system", "team-a"]}
```

A reference that crosses namespaces works only when both ends are listed:

- a ModelRouter whose `spec.agentGatewayRef.namespace` names another namespace
- a LoRAAdapter whose `spec.inferenceServiceRef.namespace` names another namespace
- a GPUQuota whose `namespaceRef` or `selector` covers another namespace

## Adding or removing a namespace

Change the list and run `helm upgrade`. The chart adds or removes the Role and
RoleBinding, and the controller restarts with the new flag. Removing a
namespace does not delete the workloads in it; they keep running and are no
longer reconciled.

## Checking the result

```bash
SA=system:serviceaccount:llmkube-system:$(kubectl get deploy -n llmkube-system \
  -l app.kubernetes.io/name=llmkube -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}')

kubectl auth can-i list deployments -n team-a --as "$SA"    # yes
kubectl auth can-i list deployments -n default --as "$SA"   # no
kubectl auth can-i list secrets --all-namespaces --as "$SA" # no
kubectl auth can-i list nodes --as "$SA"                    # yes
```

## Without the chart

The flag works on its own: `--watch-namespaces=team-a,team-b`. The kustomize
manifests under `config/` still bind the generated ClusterRole, so the cache
is scoped but the grants are not. To scope the grants as well, bind the
namespaced rules through a Role in each namespace the way the chart's
`manager-namespaced-rbac.yaml` does.
