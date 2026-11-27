package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Every usage row of a record carries its request ID, so a request ID
// longer than MaxRequestIDLen is stored as its hash, never refused (a
// client can choose LiteLLM's call ID, and a refused record is billed
// nowhere): pushed and pulled copies get the same ID and count once. An ID
// that looks like such a hash is hashed too, so no other request can take
// it.
func TestPushedRequestIDsAreBounded(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(-30 * time.Second)
	long := strings.Repeat("r", MaxRequestIDLen+1)
	res := r.post(t, Native, native(long, "acme", at, 1, 0))
	if res.Accepted != 1 || res.RejectedCount != 0 {
		t.Fatalf("a %d-byte request ID: %+v", MaxRequestIDLen+1, res)
	}
	ids := storedRequestIDs(t, r)
	if want := RequestID(long); !ids[want] || len(want) > MaxRequestIDLen || !strings.HasPrefix(want, "sha256:") {
		t.Fatalf("stored request IDs %v, want %s", ids, want)
	}
	obj := map[string]any{}
	_ = json.Unmarshal([]byte(native(long, "acme", at, 1, 0)), &obj)
	if pres, err := r.in.ingestPulled(Native, Native, "", []map[string]any{obj}, nil); err != nil || pres.AlreadyStored != 1 {
		t.Fatalf("the pulled copy of the same request: %+v %v, want it already stored", pres, err)
	}
	if res := r.post(t, Native, native(strings.Repeat("r", MaxRequestIDLen), "acme", at, 1, 0)); res.Accepted != 1 {
		t.Fatalf("a %d-byte request ID: %+v", MaxRequestIDLen, res)
	}
	if res := r.post(t, Native, native(RequestID(long), "acme", at, 7, 0)); res.Accepted != 1 || res.AlreadyStored != 0 {
		t.Fatalf("a request whose ID is the hash a long one is stored under must not count as that request: %+v", res)
	}
}

// storedRequestIDs returns the request IDs of the records stored.
func storedRequestIDs(t *testing.T, r *rig) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := r.records.ScanRecords(t0.Add(-time.Hour), t0.Add(time.Hour), func(e *usage.Event, _ time.Time) error {
		id, _ := e.Properties["request_id"].(string)
		out[id] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// One push writes at most MaxStoredPerRequest to the records ledger: every
// usage row repeats the record's dimensions, so a small body of records
// with many metrics and dimensions would be stored many times over.
func TestPushedRecordsTooLargeToStoreAreRefused(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	// Fifteen metrics an emitter may push under (the ones vBilling measures
	// itself take no records).
	var metrics []string
	for i := 0; i < 15; i++ {
		code := fmt.Sprintf("bulk_metric_%02d", i)
		if err := usage.RegisterCustom(usage.MetricDef{Code: code, Unit: "units"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { usage.RemoveCustom(code) })
		metrics = append(metrics, code)
	}
	at := t0.Add(-30 * time.Second).Format(time.RFC3339Nano)
	dims := map[string]any{}
	for i := 0; i < usage.MaxDimensions; i++ {
		dims[fmt.Sprintf("d%02d", i)] = strings.Repeat("v", 200)
	}
	use := map[string]any{}
	for _, m := range metrics {
		use[m] = 1
	}
	var b strings.Builder
	n := 0
	for size := 0; size <= MaxStoredPerRequest; size += len(metrics) * (450 + usage.MaxDimensions*209) {
		line, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("r-%d", n), "tenant": "acme", "time": at, "usage": use, "dimensions": dims})
		b.Write(line)
		b.WriteByte('\n')
		n++
	}
	if b.Len() > 16<<20 {
		t.Fatalf("test setup: %d bytes is more than the API takes", b.Len())
	}
	_, err := r.in.Ingest(Native, Native, "", []byte(b.String()))
	var tooLarge ErrTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("%d records of %d metrics in a %d-byte body: %v, want ErrTooLarge", n, len(metrics), b.Len(), err)
	}
	if st := r.records.Stats(); st.Committed != 0 {
		t.Fatalf("nothing may be stored: %+v", st)
	}
}
