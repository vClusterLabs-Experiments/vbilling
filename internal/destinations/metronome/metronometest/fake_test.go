package metronometest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
)

var day = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

func client(f *metronometest.Server) *metronome.Client {
	return metronome.NewClient(f.URL, metronometest.Token)
}

func event(id, customer string, at time.Time, value string) metronome.IngestEvent {
	return metronome.IngestEvent{TransactionID: id, CustomerID: customer, EventType: "vcluster_gpu_hours", Timestamp: at.Format(time.RFC3339),
		Properties: map[string]string{"value": value, "region": "ap-southeast-2", "sku": "H100", "capacity_type": "on-demand", "billing_mode": "shared", "tenant_cluster": "c1"}}
}

// Usage sent under an alias before any customer has it is kept and attached
// to the customer that gets the alias later; a repeated transaction_id is
// dropped; usage aggregates per billable metric and UTC day.
func TestIngestDedupeAndLateAttachment(t *testing.T) {
	f := metronometest.New(t)
	bm := f.AddBillableMetric([]byte(`{"name":"GPU","aggregation_type":"SUM","aggregation_key":"value",
		"event_type_filter":{"in_values":["vcluster_gpu_hours"]},"property_filters":[{"name":"value","exists":true}]}`))
	c := client(f)
	ctx := context.Background()
	evs := []metronome.IngestEvent{event("t1", "tenant-a", day.Add(time.Hour), "0.5"), event("t2", "tenant-a", day.Add(2*time.Hour), "0.25")}
	if err := c.Ingest(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := c.Ingest(ctx, append(evs, event("t1", "tenant-a", day.Add(3*time.Hour), "9"))); err != nil { // a replay
		t.Fatal(err)
	}
	got := f.Events()
	if len(got) != 2 || got[0].Customer != "" {
		t.Fatalf("events %+v: want 2, unattributed", got)
	}
	cus, err := f.CreateCustomer("Acme", "org_1", "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Events() {
		if e.Customer != cus {
			t.Fatalf("event %s not attached to the customer that took the alias", e.TransactionID)
		}
	}
	rows, err := c.Usage(ctx, metronome.UsageRequest{StartingOn: day.Format(time.RFC3339), EndingBefore: day.Add(48 * time.Hour).Format(time.RFC3339),
		WindowSize: "day", CustomerIDs: []string{cus}, BillableMetrics: []map[string]any{{"id": bm}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Value.String() != "0.75" || rows[1].Value.String() != "0" {
		t.Fatalf("usage rows %+v", rows)
	}
}

// An ingest alias belongs to one customer, archived ones included: creating
// another customer with it answers 409.
func TestAliasConflicts(t *testing.T) {
	f := metronometest.New(t)
	ctx := context.Background()
	old, _ := f.CreateCustomer("Churned", "tenant-old")
	f.ArchiveCustomer(old)
	if _, err := f.CreateCustomer("Again", "tenant-old"); !errors.Is(err, metronometest.ErrAliasInUse) {
		t.Fatalf("archived holder: %v", err)
	}
	_, err := client(f).CreateCustomer(ctx, metronome.CreateCustomerRequest{Name: "x", IngestAliases: []string{"tenant-old"}})
	if !metronome.IsConflict(err) {
		t.Fatalf("POST /v1/customers: want 409, got %v", err)
	}
	if cus, err := client(f).FindCustomerByAlias(ctx, "tenant-old"); err != nil || cus != nil {
		t.Fatalf("archived customers are not listed by default: %+v %v", cus, err)
	}
	if cus, err := client(f).FindArchivedCustomerByAlias(ctx, "tenant-old"); err != nil || cus == nil || cus.ID != old {
		t.Fatalf("only_archived: %+v %v", cus, err)
	}
	if len(f.Writes()) != 0 {
		t.Fatalf("writes %v", f.Writes())
	}
}

// Webhooks are signed the way vBilling verifies them.
func TestWebhooksAreSignedLikeMetronome(t *testing.T) {
	f := metronometest.New(t)
	now := time.Now()
	var mu sync.Mutex
	var verified []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		err := metronome.VerifySignature(body, r.Header.Get("X-Metronome-Date"), r.Header.Get("Metronome-Webhook-Signature"), "whsec", metronome.DefaultTolerance, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		verified = append(verified, string(body))
		mu.Unlock()
	}))
	defer srv.Close()
	cus, _ := f.CreateCustomer("Acme", "org_1", "tenant-a")
	for _, n := range []metronometest.Notification{
		f.Alert("alerts.low_remaining_commit_balance_reached", cus, "Prepaid balance low", 10, 50),
		f.PaymentStatus(cus, "contract-1", "paid"),
	} {
		status, body, err := f.SendWebhook(srv.URL, "whsec", n)
		if err != nil || status != http.StatusOK {
			t.Fatalf("%s: %d %s %v", n.Type, status, body, err)
		}
	}
	if status, _, _ := f.SendWebhook(srv.URL, "wrong-secret", f.PaymentStatus(cus, "contract-1", "paid")); status != http.StatusBadRequest {
		t.Fatalf("a wrong secret verified: %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(verified) != 2 || !strings.Contains(verified[0], `"customer_id":"`+cus+`"`) || !strings.Contains(verified[1], `"payment_status":"paid"`) {
		t.Fatalf("payloads: %v", verified)
	}
	if metronometest.Sign([]byte("b"), "d", "s") != metronome.Sign([]byte("b"), "d", "s") {
		t.Fatal("signature schemes differ")
	}
}

// recorder keeps the fake's test failures instead of failing the test.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// A body that does not match Metronome's schema is refused (400) and named.
func TestFakeRejectsBodiesOutsideTheSchema(t *testing.T) {
	rec := &recorder{TB: t}
	f := metronometest.New(rec)
	req, _ := http.NewRequest(http.MethodPost, f.URL+"/v1/customers", bytes.NewReader([]byte(`{"name":"x","ingest_alias":["typo"]}`)))
	req.Header.Set("Authorization", "Bearer "+metronometest.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.errs) == 0 || !strings.Contains(strings.Join(rec.errs, "\n"), `unknown field "ingest_alias"`) {
		t.Fatalf("violations %v", rec.errs)
	}
}
