package records

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A pushed record's values and dimensions are checked: each usage row of
// the record carries them, and billing platforms receive them next to the
// usage's own properties. A pulled one is repaired instead.
func TestPushedRecordValuesAndDimensions(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(-30 * time.Second).Format(time.RFC3339Nano)
	rec := func(id string, extra map[string]any) string {
		m := map[string]any{"id": id, "tenant": "acme", "time": at, "usage": map[string]any{"tokens_in": 1}}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	many := map[string]any{}
	for i := 0; i <= usage.MaxDimensions; i++ {
		many[fmt.Sprintf("d%02d", i)] = "x"
	}
	for name, body := range map[string]string{
		"reserved dimension": rec("r-1", map[string]any{"dimensions": map[string]any{"value": "0"}}),
		"sku as dimension":   rec("r-2", map[string]any{"dimensions": map[string]any{"sku": "free"}}),
		"too many":           rec("r-3", map[string]any{"dimensions": many}),
		"region markup":      rec("r-4", map[string]any{"region": "<b>eu</b>"}),
		"project control":    rec("r-5", map[string]any{"project": "a\nb"}),
	} {
		if res := r.post(t, Native, body); res.Accepted != 0 || len(res.Rejected) != 1 {
			t.Errorf("%s: %+v, want the record refused", name, res)
		}
	}
	if res := r.post(t, Native, rec("r-ok", map[string]any{"dimensions": map[string]any{"endpoint": "chat"}, "region": "eu-west-1"})); res.Accepted != 1 {
		t.Fatalf("a plain record: %+v", res)
	}

	// Pulled from a gateway's own log: repaired, not refused.
	var obj map[string]any
	_ = json.Unmarshal([]byte(rec("p-1", map[string]any{"dimensions": map[string]any{"value": "0", "endpoint": "chat"}, "region": "<eu>"})), &obj)
	if res, err := r.in.ingestPulled(Native, "gw", "", []map[string]any{obj}, nil); err != nil || res.Accepted != 1 {
		t.Fatalf("pulled: %+v %v", res, err)
	}
	_ = r.records.ScanRecords(t0.Add(-time.Hour), t0.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		if ev.Properties["request_id"] == "p-1" && (ev.Dim("value") != "" || ev.Dim("endpoint") != "chat" || ev.Region != "_eu_") {
			t.Errorf("pulled row %+v: the reserved dimension must go and the region be repaired", ev)
		}
		return nil
	})
}

// A format may not name a dimension after a property of the usage itself.
func TestFormatsRefuseReservedDimensions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "formats.yaml")
	if err := os.WriteFile(path, []byte("kong:\n  id: request.id\n  time: started_at\n  tenant: consumer.id\n  usage: {tokens_in: tokens}\n  dimensions: {value: route.name}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFormats(path, false)
	if err == nil || !strings.Contains(err.Error(), "value") {
		t.Fatalf("a dimension named value: %v", err)
	}
	// The refusal lists every reserved name, metering_source included
	// (the lists in the docs and in this message left it out).
	for _, name := range []string{"value", "unit", "sku", "region", "project", "resource_id", "source", "window_start", "window_end", "schema_version",
		"accuracy", "estimate", "metering_source"} {
		if !usage.ReservedDimension(name) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: reserved %v, named in the refusal %v", name, usage.ReservedDimension(name), strings.Contains(err.Error(), name))
		}
	}
}
