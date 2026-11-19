package records

import (
	"encoding/json"
	"testing"
	"time"
)

// capLlama stores "llama" as "capped" and records which source asked.
type capLlama struct {
	sources  map[string]bool
	overflow map[string]int // source/tenant -> rows reported
}

func (g *capLlama) Admit(source, tenant, sku string) (string, bool) {
	g.sources[source] = true
	if sku == "llama" {
		return "capped", true
	}
	return sku, false
}

func (g *capLlama) Overflowed(source, tenant string, n int) { g.overflow[source+"/"+tenant] += n }

// The SKU gate (the API's SKU cap) applies to every stored row: rows past
// the cap are stored under the gate's SKU, never dropped, and reported
// once per request. Each kind of source counts on its own.
func TestSKUGateAppliesToRecords(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	g := &capLlama{sources: map[string]bool{}, overflow: map[string]int{}}
	r.in.UseSKUGate(g)
	res := r.post(t, Native, native("r-1", "acme", t0.Add(-30*time.Second), 3, 4))
	if res.Accepted != 1 || len(res.Warnings) == 0 {
		t.Fatalf("result: %+v", res)
	}
	if sku := storedSKUs(t, r)["r-1"]; sku != "capped" {
		t.Fatalf("stored SKU %q, want the gate's", sku)
	}
	if g.overflow[SKUSourceShared+"/acme"] != 2 || len(g.overflow) != 1 {
		t.Fatalf("overflow reported %v, want both rows of acme's record once, from the shared token", g.overflow)
	}
	if _, err := r.in.Ingest(Native, Native, "globex", []byte(native("r-2", "globex", t0.Add(-30*time.Second), 3, 0))); err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{}
	_ = json.Unmarshal([]byte(native("r-3", "acme", t0.Add(-30*time.Second), 3, 0)), &obj)
	if _, err := r.in.ingestPulled(Native, "gw", "", []map[string]any{obj}, nil); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{SKUSourceShared, SKUSourceBound, SKUSourcePull("gw")} {
		if !g.sources[src] {
			t.Errorf("no row counted against %s (asked: %v)", src, g.sources)
		}
	}
}
