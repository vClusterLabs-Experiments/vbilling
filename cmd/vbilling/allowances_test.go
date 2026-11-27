package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// ALLOWANCES_FILE is read and checked as start-up reads it: check-config
// lists it with what it changes, and refuses an invalid one with the error
// start-up stops with.
func TestCheckConfigReadsAllowances(t *testing.T) {
	good := write(t, "allowances.yaml", "- {metric: vcluster_storage_gb_hours, included: 32, per: node}\n")
	r := vbilling(t, map[string]string{"ADAPTERS": "noop", "ALLOWANCES_FILE": good}, "check-config")
	want := "ok       ALLOWANCES_FILE " + good + ": 1 allowance(s): storage is billed as vcluster_storage_overage_gb_hours, the storage above them; " +
		"vcluster_storage_gb_hours is informational\n"
	if r.code != 0 || !strings.Contains(r.stdout, want) {
		t.Errorf("check-config: exit %d, want 0 and\n  %s%s%s", r.code, want, r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout, "warning  ALLOWANCES_FILE") {
		t.Errorf("no warning with nodes billed whole:\n%s", r.stdout)
	}

	// Per node counts nodes billed whole: with PRIVATE_NODE_BILLING=usage the
	// clusters with their own nodes include nothing. A warning, not an error.
	r = vbilling(t, map[string]string{"ADAPTERS": "noop", "ALLOWANCES_FILE": good, "PRIVATE_NODE_BILLING": "usage"}, "check-config")
	if r.code != 0 || !strings.Contains(r.stdout, "warning  ALLOWANCES_FILE: an allowance per node counts the nodes billed whole, and PRIVATE_NODE_BILLING=usage") {
		t.Errorf("check-config with PRIVATE_NODE_BILLING=usage: exit %d, want 0 with a warning\n%s%s", r.code, r.stdout, r.stderr)
	}

	missing := filepath.Join(t.TempDir(), "not-there.yaml")
	for _, tc := range []struct{ name, content, path, want string }{
		{name: "a metric that takes no allowance", content: "- {metric: vcluster_gpu_hours, included: 1, per: node}\n",
			want: `ALLOWANCES_FILE: {file}: allowance 1: metric "vcluster_gpu_hours" takes no allowance: only vcluster_storage_gb_hours does`},
		{name: "an allowance per something else", content: "- {metric: vcluster_storage_gb_hours, included: 32, per: instance}\n",
			want: `ALLOWANCES_FILE: {file}: allowance 1: per "instance" must be node, cluster or volume`},
		{name: "nothing included", content: "- {metric: vcluster_storage_gb_hours, included: 0, per: cluster}\n",
			want: `ALLOWANCES_FILE: {file}: allowance 1: included must be a number of GiB above 0, not 0`},
		{name: "a class with two allowances",
			content: "- {metric: vcluster_storage_gb_hours, included: 32, per: node, storageClasses: [fast]}\n- {metric: vcluster_storage_gb_hours, included: 8, per: volume, storageClasses: [fast]}\n",
			want:    `ALLOWANCES_FILE: {file}: allowance 2: storage class "fast" is in allowance 1 too: a class has one allowance`},
		{name: "an unknown field", content: "- {metric: vcluster_storage_gb_hours, included: 32, per: node, free: 32}\n",
			want: `ALLOWANCES_FILE: {file}: error unmarshaling JSON: while decoding JSON: json: unknown field "free"`},
		{name: "a file that is not there", path: missing, want: "ALLOWANCES_FILE: open " + missing + ": no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = write(t, "allowances.yaml", tc.content)
			}
			want := strings.ReplaceAll(tc.want, "{file}", path)
			env := map[string]string{"ADAPTERS": "noop", "ALLOWANCES_FILE": path}
			check := vbilling(t, env, "check-config")
			if check.code != 1 || !strings.Contains(check.stderr, "error    "+want+"\n") {
				t.Errorf("check-config: exit %d, want 1 with\n  %s\nstderr:\n%s", check.code, want, check.stderr)
			}
			start := vbilling(t, env)
			if start.code != 1 || !strings.Contains(start.stderr, ": "+want+"\n") || strings.Contains(start.stderr, "kubernetes config") {
				t.Errorf("start-up: exit %d, want 1 with the same text before it looks for a cluster\n  %s\nstderr:\n%s", start.code, want, start.stderr)
			}
		})
	}
}

// The definitions printed for Metronome follow the allowances: the overage
// metric, not the whole capacity, which billing platforms do not get.
func TestMetronomeBillableMetricsWithAllowances(t *testing.T) {
	t.Setenv("ALLOWANCES_FILE", write(t, "allowances.yaml", "- {metric: vcluster_storage_gb_hours, included: 32, per: cluster}\n"))
	t.Cleanup(func() { usage.UseStorageAllowances(false) })
	var out, errb bytes.Buffer
	if code := metronomeMain([]string{"billable-metrics"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var defs []map[string]any
	if err := json.Unmarshal(out.Bytes(), &defs); err != nil {
		t.Fatal(err)
	}
	types := map[string]map[string]any{}
	for _, d := range defs {
		types[d["event_type_filter"].(map[string]any)["in_values"].([]any)[0].(string)] = d
	}
	over := types[usage.MetricStorageOverageGBHours]
	if over == nil || types[usage.MetricStorageGBHours] != nil {
		t.Fatalf("billable metrics for %v, want the overage and not the whole capacity", keys(types))
	}
	if keys, _ := json.Marshal(over["group_keys"]); string(keys) != `[["region","sku","tenant_cluster"]]` {
		t.Errorf("overage group keys %s, want those of storage", keys)
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
