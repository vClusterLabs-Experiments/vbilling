package enforcement

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

type recordingGateway struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingGateway) Apply(_ context.Context, tenant string, blocked bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%s=%t", tenant, blocked))
	return nil
}

func (r *recordingGateway) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[len(r.calls)-1]
}

// A tenant that only buys tokens has no cluster: suspending it must still
// stop its requests, at the gateway, and only in enforce mode.
func TestGatewayFollowsBillingStates(t *testing.T) {
	ctx := context.Background()
	gw := &recordingGateway{}
	e := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: time.Now, Gateway: gw})
	for _, step := range []struct {
		state State
		want  string
	}{
		{Warning, "tokens-only=false"},
		{Suspended, "tokens-only=true"},
		{Paused, "tokens-only=true"},
		{Delinquent, "tokens-only=false"},
		{Active, "tokens-only=false"},
	} {
		if err := e.Set(ctx, "tokens-only", step.state, "test", "api", "manual"); err != nil {
			t.Fatal(err)
		}
		if got := gw.last(); got != step.want {
			t.Errorf("%s: gateway %q, want %q", step.state, got, step.want)
		}
	}
	// Resync converges every tenant again (retries a failed block).
	_ = e.Set(ctx, "globex", Suspended, "card declined", "api", "manual")
	n := len(gw.calls)
	e.Resync(ctx)
	if len(gw.calls)-n != 2 {
		t.Errorf("resync applied %v, want both tenants", gw.calls[n:])
	}

	annotate := &recordingGateway{}
	a := New(fake.NewSimpleClientset(), Options{Mode: "annotate", Now: time.Now, Gateway: annotate})
	_ = a.Set(ctx, "acme", Suspended, "test", "api", "manual")
	a.Resync(ctx)
	if len(annotate.calls) != 0 {
		t.Errorf("annotate mode touched the gateway: %v", annotate.calls)
	}
}
