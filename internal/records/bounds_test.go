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

// Every usage row of a record carries its request ID, so a pushed
// record's request ID is bounded, and a refusal does not echo it whole.
func TestPushedRequestIDsAreBounded(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(-30 * time.Second)
	res := r.post(t, Native, native(strings.Repeat("r", MaxRequestIDLen+1), "acme", at, 1, 0))
	if res.Accepted != 0 || len(res.Rejected) != 1 || len(res.Rejected[0].ID) > MaxRequestIDLen+3 {
		t.Fatalf("a %d-byte request ID: %+v", MaxRequestIDLen+1, res)
	}
	if res := r.post(t, Native, native(strings.Repeat("r", MaxRequestIDLen), "acme", at, 1, 0)); res.Accepted != 1 {
		t.Fatalf("a %d-byte request ID: %+v", MaxRequestIDLen, res)
	}
}

// One push writes at most MaxStoredPerRequest to the records ledger: every
// usage row repeats the record's dimensions, so a small body of records
// with many metrics and dimensions would be stored many times over.
func TestPushedRecordsTooLargeToStoreAreRefused(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	var metrics []string
	for _, def := range usage.Catalog() {
		metrics = append(metrics, def.Code)
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
