package enforcement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type resolver map[string]string

func (r resolver) TenantForCustomer(_ context.Context, id string) (string, bool) {
	t, ok := r[id]
	return t, ok
}

func setup(t *testing.T, mode string, rules map[string]State) (*Enforcer, *fake.Clientset, *kv.Store) {
	t.Helper()
	kube := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: "team-a"}},
	)
	state := kv.Memory()
	e := New(kube, Options{
		Mode: mode, StripeWebhookSecret: "whsec_test", MetronomeWebhookSecret: "mtr_secret", Rules: rules, State: state,
		Clusters: func(tenant string) []discovery.TenantCluster {
			if tenant != "acme" {
				return nil
			}
			return []discovery.TenantCluster{{Name: "gpu", Namespace: "team-a", Source: "statefulset"}}
		},
		Resolvers: map[string]destinations.CustomerResolver{"stripe": resolver{"cus_acme": "acme"}, "metronome": resolver{"5f794d50-085a-4db6-8d15-286e518b7225": "acme"}},
		Now:       func() time.Time { return now },
	})
	return e, kube, state
}

func stripeEvent(t *testing.T, e *Enforcer, id, typ, status string) (Signal, error) {
	body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"data":{"object":{"customer":"cus_acme","status":%q,"attempt_count":2}}}`, id, typ, status))
	return e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now))
}

func nsLabels(t *testing.T, kube *fake.Clientset) map[string]string {
	ns, err := kube.CoreV1().Namespaces().Get(context.Background(), "team-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ns.Labels
}

func TestDunningLifecycleInEnforceMode(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)

	sig, err := stripeEvent(t, e, "evt_1", "invoice.payment_failed", "")
	if err != nil || sig.State != Delinquent || sig.Tenant != "acme" {
		t.Fatalf("payment_failed: %+v %v", sig, err)
	}
	sts, _ := kube.AppsV1().StatefulSets("team-a").Get(context.Background(), "gpu", metav1.GetOptions{})
	if sts.Annotations[AnnotationState] != "delinquent" || !strings.Contains(sts.Annotations[AnnotationWhy], "attempt 2") {
		t.Fatalf("workload annotations: %v", sts.Annotations)
	}
	if l := nsLabels(t, kube); l[LabelState] != "delinquent" || l[LabelSuspended] != "" {
		t.Fatalf("delinquent must not suspend: %v", l)
	}

	if _, err := stripeEvent(t, e, "evt_2", "customer.subscription.updated", "unpaid"); err != nil {
		t.Fatal(err)
	}
	if l := nsLabels(t, kube); l[LabelSuspended] != "true" || l[LabelState] != "suspended" {
		t.Fatalf("unpaid must suspend in enforce mode: %v", l)
	}

	if _, err := stripeEvent(t, e, "evt_3", "invoice.paid", ""); err != nil {
		t.Fatal(err)
	}
	if l := nsLabels(t, kube); l[LabelSuspended] != "" || l[LabelState] != "active" {
		t.Fatalf("paid invoice must lift suspension: %v", l)
	}
	events, _ := kube.CoreV1().Events("team-a").List(context.Background(), metav1.ListOptions{})
	if len(events.Items) != 3 {
		t.Fatalf("kubernetes events: %d", len(events.Items))
	}
}

func TestAnnotateModeNeverSuspendsAndObserveTouchesNothing(t *testing.T) {
	e, kube, _ := setup(t, "annotate", nil)
	if _, err := stripeEvent(t, e, "evt_1", "customer.subscription.deleted", ""); err != nil {
		t.Fatal(err)
	}
	if l := nsLabels(t, kube); l[LabelState] != "suspended" || l[LabelSuspended] != "" {
		t.Fatalf("annotate mode labels: %v", l)
	}

	o, okube, _ := setup(t, "observe", nil)
	if _, err := stripeEvent(t, o, "evt_1", "customer.subscription.deleted", ""); err != nil {
		t.Fatal(err)
	}
	if l := nsLabels(t, okube); len(l) != 0 {
		t.Fatalf("observe mode must not write to the cluster: %v", l)
	}
	if o.StateOf("acme").State != Suspended {
		t.Fatal("observe mode must still record the state")
	}
}

func TestSignatureDedupeAndIrrelevantEvents(t *testing.T) {
	e, _, _ := setup(t, "enforce", nil)
	body := []byte(`{"id":"evt_x","type":"invoice.payment_failed","data":{"object":{"customer":"cus_acme"}}}`)
	if _, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "wrong", now)); err == nil {
		t.Fatal("bad signature accepted")
	}
	if _, err := stripeEvent(t, e, "evt_9", "customer.created", ""); !errors.Is(err, ErrIgnored) {
		t.Fatalf("irrelevant event: %v", err)
	}
	if _, err := stripeEvent(t, e, "evt_1", "invoice.payment_failed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := stripeEvent(t, e, "evt_2", "invoice.paid", ""); err != nil {
		t.Fatal(err)
	}
	// A late redelivery of the old failure must not flip the tenant back.
	if _, err := stripeEvent(t, e, "evt_1", "invoice.payment_failed", ""); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("redelivery: %v", err)
	}
	if got := e.StateOf("acme").State; got != Active {
		t.Fatalf("redelivered event re-applied: %s", got)
	}
	body = []byte(`{"id":"evt_3","type":"invoice.payment_failed","data":{"object":{"customer":"cus_stranger"}}}`)
	if _, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now)); err == nil {
		t.Fatal("unknown customer accepted")
	}
}

func TestMetronomeAlertsAndRules(t *testing.T) {
	rules, err := ParseRules("alerts.spend_threshold_reached=suspended")
	if err != nil {
		t.Fatal(err)
	}
	e, kube, state := setup(t, "enforce", rules)
	send := func(id, typ, props string) (Signal, error) {
		body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"%s}}`, id, typ, props))
		date := now.Format(http.TimeFormat)
		return e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret"))
	}
	sig, err := send("a1", "alerts.low_remaining_contract_credit_balance_reached", `,"alert_name":"POC credits below 10%"`)
	if err != nil || sig.State != Warning || sig.Reason != "POC credits below 10%" {
		t.Fatalf("credit alert: %+v %v", sig, err)
	}
	sig, err = send("a2", "alerts.spend_threshold_reached", "")
	if err != nil || sig.State != Suspended {
		t.Fatalf("rule override: %+v %v", sig, err)
	}
	if l := nsLabels(t, kube); l[LabelSuspended] != "true" {
		t.Fatalf("spend cap rule must suspend: %v", l)
	}
	// States survive restarts.
	if raw, ok := state.Get("state/acme"); !ok || !strings.Contains(raw, "suspended") {
		t.Fatalf("persisted state: %q", raw)
	}
	reloaded := New(kube, Options{Mode: "enforce", State: state})
	if reloaded.StateOf("acme").State != Suspended {
		t.Fatal("state not restored from disk")
	}
	if _, err := ParseRules("x=bogus"); err == nil {
		t.Fatal("invalid rule accepted")
	}
}
