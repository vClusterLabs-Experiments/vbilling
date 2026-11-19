package safeguard

import (
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var ws = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// wholeNode is what the collector bills for a node rented whole for the
// window: node-hours and its physical GPUs, to one cluster's tenant.
func wholeNode(tenant, node string, start time.Time, gpus, share float64) []usage.Event {
	h := time.Minute.Hours() * share
	n := usage.Event{Tenant: tenant, Metric: usage.MetricPrivateNodeHours, Quantity: h, ResourceID: node, WindowStart: start, WindowEnd: start.Add(time.Minute),
		Dimensions: map[string]string{usage.DimNode: node, usage.DimBillingMode: "private_node"}}
	g := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: gpus * h, ResourceID: node, WindowStart: start, WindowEnd: start.Add(time.Minute),
		Dimensions: map[string]string{usage.DimNode: node, usage.DimGPUProfile: "full"}, Properties: map[string]any{"physical_gpus": gpus}}
	n.Finalize()
	g.Finalize()
	return []usage.Event{n, g}
}

func TestCheck(t *testing.T) {
	ok := append(wholeNode("acme", "gpu-1", ws, 8, 1), wholeNode("globex", "gpu-2", ws, 4, 0.5)...)
	// A pod's GPU on a shared node names its cluster, not the node: not checked per node.
	pod := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 1, ResourceID: "vcluster-a-train", WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	pod.Finalize()
	if f, _ := Check(ws, ws.Add(time.Minute), append(ok, pod)); len(f) != 0 {
		t.Fatalf("findings in a window within bounds: %+v", f)
	}
	// gpu-1 is billed whole to two clusters of different tenants.
	twice := append(wholeNode("acme", "gpu-1", ws, 8, 1), wholeNode("globex", "gpu-1", ws, 8, 1)...)
	f, _ := Check(ws, ws.Add(time.Minute), twice)
	got := map[string]string{}
	for _, x := range f {
		got[x.Check] = x.Subject
	}
	if len(f) != 3 || got[NodeHours] != "gpu-1" || got[GPUHours] != "gpu-1" || got[ResourceTenants] != "gpu-1" {
		t.Fatalf("findings %+v: want gpu-1 over its node-hours and GPU-hours, billed to two tenants", f)
	}
	// A partial window (realigning to the grid) holds proportionally less.
	short := wholeNode("acme", "gpu-1", ws, 8, 1) // a full minute billed in 45 seconds
	if f, _ := Check(ws, ws.Add(45*time.Second), short); len(f) != 2 {
		t.Fatalf("a minute of node billed in a 45-second window: %+v", f)
	}
}

// The checker looks at the windows new ledger records touch, from every
// source, turns an offending node's health diverged and counts it once,
// and clears it once a later window is within bounds. It never holds
// delivery: the ledger is untouched.
func TestCheckerFlagsAndClears(t *testing.T) {
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: func() time.Time { return ws.Add(5 * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	c := New(sp, h, tel)

	// The collector bills gpu-1 to acme; a fill later bills it to globex too.
	if err := sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), wholeNode("acme", "gpu-1", ws, 8, 1), ""); err != nil {
		t.Fatal(err)
	}
	if f := c.Once(); len(f) != 0 {
		t.Fatalf("findings before the fill: %+v", f)
	}
	if _, _, err := sp.AppendIngest("tenant-api-fill", wholeNode("globex", "gpu-1", ws, 8, 1)); err != nil {
		t.Fatal(err)
	}
	if f := c.Once(); len(f) != 3 {
		t.Fatalf("findings after the fill: %+v", f)
	}
	c.Once() // nothing new: nothing counted twice
	var diverged []string
	for _, s := range h.List() {
		if s.Status == health.Diverged && s.Kind == "safeguard" {
			diverged = append(diverged, s.Name)
		}
	}
	if len(diverged) != 3 {
		t.Fatalf("diverged sources %v, want gpu-1 under all three checks", diverged)
	}
	var text strings.Builder
	tel.WriteText(&text)
	if !strings.Contains(text.String(), `vbilling_safeguard_violations_total{check="node_hours"} 1`) {
		t.Fatalf("violation not counted once:\n%s", text.String())
	}
	// The next window bills gpu-1 once: back within bounds.
	if err := sp.AppendWindow(spool.SourceCollector, ws.Add(2*time.Minute), wholeNode("acme", "gpu-1", ws.Add(time.Minute), 8, 1), ""); err != nil {
		t.Fatal(err)
	}
	c.Once()
	for _, s := range h.List() {
		if s.Kind == "safeguard" && s.Status != health.OK {
			t.Errorf("%s still %s after a window within bounds", s.Name, s.Status)
		}
	}
}
