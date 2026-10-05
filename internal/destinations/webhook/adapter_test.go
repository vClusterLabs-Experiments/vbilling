package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestSendsSignedCloudEventsBatches(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var got [][]CloudEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if ct := r.Header.Get("Content-Type"); ct != "application/cloudevents-batch+json" {
			t.Errorf("content type %q", ct)
		}
		if r.Header.Get("X-Tenant-Platform") != "dataplane" {
			t.Error("custom header missing")
		}
		// The signature scheme is Stripe-compatible, so Stripe's verifier works.
		if err := stripe.VerifySignature(body, r.Header.Get(SignatureHeader), "s3cret", 5*time.Minute, now); err != nil {
			t.Errorf("signature: %v", err)
		}
		var batch []CloudEvent
		json.Unmarshal(body, &batch)
		got = append(got, batch)
	}))
	defer srv.Close()

	a := New(&config.Config{WebhookURL: srv.URL, WebhookSecret: "s3cret", Region: "ap-southeast-2", ClusterName: "syd-cp-1",
		WebhookHeaders: map[string]string{"X-Tenant-Platform": "dataplane"}})
	a.now = func() time.Time { return now }

	var events []usage.Event
	for i := 0; i < 1200; i++ {
		ws := now.Add(-time.Duration(i+1) * time.Minute)
		e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUUtilization, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
		e.Finalize()
		events = append(events, e)
	}
	if err := a.SendEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || len(got[0]) != 500 || len(got[2]) != 200 {
		t.Fatalf("batches: %d", len(got))
	}
	ce := got[0][0]
	if ce.SpecVersion != "1.0" || ce.Type != TypeUsage || ce.ID != events[0].ID || ce.Subject != "acme" ||
		ce.Source != "vbilling/ap-southeast-2/syd-cp-1" || ce.DataSchema != DataSchema {
		t.Fatalf("envelope: %+v", ce)
	}
	// Webhooks are a data feed: informational metrics are included.
	if destinations.Accepts(a, usage.MetricGPUUtilization) != true {
		t.Fatal("webhook should accept every metric")
	}
}

func TestErrorClassification(t *testing.T) {
	status := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	a := New(&config.Config{WebhookURL: srv.URL})
	ev := usage.Event{Tenant: "a", Metric: usage.MetricGPUHours, Quantity: 1, WindowStart: time.Now().Add(-time.Minute), WindowEnd: time.Now()}
	ev.Finalize()
	for code, permanent := range map[int]bool{400: true, 422: true, 401: false, 404: false, 429: false, 503: false} {
		status = code
		err := a.SendEvents(context.Background(), []usage.Event{ev})
		if err == nil || destinations.IsPermanent(err) != permanent {
			t.Errorf("HTTP %d: err=%v permanent=%v, want permanent=%v", code, err, destinations.IsPermanent(err), permanent)
		}
	}
}
