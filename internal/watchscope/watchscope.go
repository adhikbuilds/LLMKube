/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package watchscope turns the operator's --watch-namespaces flag into the
// cache scope controller-runtime needs (#1600). An operator that only ever
// serves one or two namespaces should not hold a cluster-wide cache or a
// cluster-wide ClusterRole; scoping the cache is what lets the chart bind the
// namespaced rules through per-namespace Roles instead.
package watchscope

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Parse splits a comma-separated namespace list: spaces are trimmed, empty
// entries and duplicates are dropped, and the result is sorted so the startup
// log and the rendered chart args compare equal across restarts. An empty
// result means cluster-wide, today's behaviour. A name that is not a DNS-1123
// label is an error rather than a namespace that silently never matches.
func Parse(flag string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, ns := range strings.Split(flag, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(errs, "; "))
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	sort.Strings(out)
	return out, nil
}

// DefaultNamespaces returns the cache.Options.DefaultNamespaces value for
// namespaces. Empty in gives nil, which controller-runtime reads as every
// namespace. Cluster-scoped types (Nodes, PersistentVolumes, PriorityClasses,
// Namespaces) are not narrowed by it, and a type with its own ByObject
// Namespaces entry (the watch canary's Lease) keeps that entry.
func DefaultNamespaces(namespaces []string) map[string]cache.Config {
	if len(namespaces) == 0 {
		return nil
	}
	out := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		out[ns] = cache.Config{}
	}
	return out
}

// ListScopes returns the list options an uncached reader needs to cover
// namespaces: one InNamespace per watched namespace, or a single unscoped
// list when the operator is cluster-wide. A reader that goes to the API
// server cannot list across namespaces under per-namespace Roles, so it
// lists each namespace in turn.
func ListScopes(namespaces []string) [][]client.ListOption {
	if len(namespaces) == 0 {
		return [][]client.ListOption{nil}
	}
	out := make([][]client.ListOption, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, []client.ListOption{client.InNamespace(ns)})
	}
	return out
}
