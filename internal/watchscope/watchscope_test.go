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

package watchscope

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		want    []string
		wantErr string
	}{
		{name: "empty is cluster-wide", flag: "", want: nil},
		{name: "only separators and spaces is cluster-wide", flag: " , ,", want: nil},
		{name: "single namespace", flag: "llmkube-system", want: []string{"llmkube-system"}},
		{name: "list is trimmed, deduplicated and sorted", flag: " team-b, team-a ,team-b,", want: []string{"team-a", "team-b"}},
		{name: "uppercase is rejected", flag: "team-a,Team-B", wantErr: `invalid namespace "Team-B"`},
		{name: "a wildcard is rejected", flag: "*", wantErr: `invalid namespace "*"`},
		{name: "a path is rejected", flag: "team-a/models", wantErr: `invalid namespace "team-a/models"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse(%q) error = %v, want one containing %q", tc.flag, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.flag, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse(%q) = %v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}

func TestDefaultNamespaces(t *testing.T) {
	if got := DefaultNamespaces(nil); got != nil {
		t.Errorf("DefaultNamespaces(nil) = %v, want nil so the cache stays cluster-wide", got)
	}
	got := DefaultNamespaces([]string{"team-a", "team-b"})
	if len(got) != 2 {
		t.Fatalf("DefaultNamespaces = %v, want exactly team-a and team-b", got)
	}
	for _, ns := range []string{"team-a", "team-b"} {
		if _, ok := got[ns]; !ok {
			t.Errorf("DefaultNamespaces is missing %q: %v", ns, got)
		}
	}
}

func TestListScopes(t *testing.T) {
	if got := ListScopes(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("ListScopes(nil) = %v, want one unscoped list", got)
	}
	got := ListScopes([]string{"team-a", "team-b"})
	if len(got) != 2 {
		t.Fatalf("ListScopes = %d scopes, want one per namespace", len(got))
	}
	for i, ns := range []string{"team-a", "team-b"} {
		lo := &client.ListOptions{}
		lo.ApplyOptions(got[i])
		if lo.Namespace != ns {
			t.Errorf("scope %d lists namespace %q, want %q", i, lo.Namespace, ns)
		}
	}
}
