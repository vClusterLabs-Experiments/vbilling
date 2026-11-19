package records

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A record is refused, with a reason, when a quantity is negative, not a
// number, or more than one request can use.
func TestRecordQuantitiesMustBePlausible(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	for q, want := range map[string]string{
		`-5`:             "non-negative",
		`"NaN"`:          "finite",
		`1.8e19`:         "more than one request can use",
		`4294967295`:     "more than one request can use",
		`1763532000`:     "more than one request can use", // a Unix timestamp in a token field
		`"not a number"`: "not a number",
	} {
		body := fmt.Sprintf(`{"id":"req-%d","tenant":"acme","time":%q,"usage":{"tokens_out":%s}}`, len(q), t0.Format(time.RFC3339), q)
		res, err := r.in.Ingest(Native, Native, "", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if res.Accepted != 0 || len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Error, want) {
			t.Errorf("quantity %s: %+v, want it refused (%s)", q, res, want)
		}
	}
	res := r.post(t, Native, native("req-big", "acme", t0, 0, 10_000_000)) // a whole long-context window
	if res.Accepted != 1 {
		t.Fatalf("10M tokens in one request refused: %+v", res)
	}
}
