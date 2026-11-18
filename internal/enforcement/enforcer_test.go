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
	"k8s.io/client-go/kubernetes"
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

// A payment clears the billing hold only: a suspension an operator set for
// another reason stays until the operator lifts it.
func TestHoldsAreIndependent(t *testing.T) {
	e := New(nil, Options{Mode: "observe", Now: func() time.Time { return time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC) }})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.Set(ctx, "acme", Delinquent, "invoice payment failed", "stripe", "invoice.payment_failed"))
	must(e.SetHold(ctx, "acme", "abuse", Suspended, "crypto mining", "api", "manual"))
	if s := e.StateOf("acme"); s.State != Suspended || !strings.Contains(s.Reason, "abuse") || len(s.Holds) != 2 {
		t.Fatalf("state = %+v", s)
	}
	must(e.Set(ctx, "acme", Active, "invoice paid", "stripe", "invoice.paid"))
	if s := e.StateOf("acme"); s.State != Suspended || len(s.Holds) != 1 {
		t.Fatalf("payment must not lift the abuse hold: %+v", s)
	}
	must(e.SetHold(ctx, "acme", "abuse", Active, "cleared", "api", "manual"))
	if s := e.StateOf("acme"); s.State != Active || len(s.Holds) != 0 {
		t.Fatalf("state after both cleared = %+v", s)
	}
}

// In enforce mode a suspended tenant's clusters get an admission policy
// through their own API, which is what reaches clusters with their own
// nodes; lifting the suspension removes it.
func TestSuspensionInstallsPolicyInsideTenantClusters(t *testing.T) {
	host := fake.NewSimpleClientset(
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "private-a", Namespace: "vc-private-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vc-private-a"}},
	)
	inside := fake.NewSimpleClientset()
	cl := discovery.TenantCluster{Name: "private-a", Namespace: "vc-private-a", Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", Now: time.Now,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Suspended, "subscription canceled", "stripe", "customer.subscription.deleted"); err != nil {
		t.Fatal(err)
	}
	p, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	if err != nil || !strings.Contains(p.Spec.Validations[0].Message, "subscription canceled") {
		t.Fatalf("policy = %+v, %v", p, err)
	}
	if _, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, PolicyName, metav1.GetOptions{}); err != nil {
		t.Fatalf("binding: %v", err)
	}
	// An operator hold explains itself as such, not as billing.
	if err := e.SetHold(ctx, "acme", "abuse", Suspended, "port scanning", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Active, "invoice paid", "stripe", "invoice.paid"); err != nil {
		t.Fatal(err)
	}
	p, _ = inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	if msg := p.Spec.Validations[0].Message; !strings.Contains(msg, "suspended (abuse: port scanning)") || strings.Contains(msg, "for billing") {
		t.Fatalf("operator hold message = %q", msg)
	}
	if err := e.SetHold(ctx, "acme", "abuse", Active, "reviewed", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Suspended, "subscription canceled", "stripe", "customer.subscription.deleted"); err != nil {
		t.Fatal(err)
	}
	// Suspending again (another webhook) updates the policy idempotently.
	if err := e.Set(ctx, "acme", Suspended, "still unpaid", "stripe", "invoice.payment_failed"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Active, "invoice paid", "stripe", "invoice.paid"); err != nil {
		t.Fatal(err)
	}
	if _, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{}); err == nil {
		t.Fatal("the policy must be removed when the tenant is active again")
	}
}

// A cluster created while its tenant is suspended gets the policy at the
// next resync, and one that missed the payment loses it.
func TestResyncConvergesNewAndMissedClusters(t *testing.T) {
	host := fake.NewSimpleClientset(
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "vc-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vc-a"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "vc-b"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vc-b"}},
	)
	insideA, insideB := fake.NewSimpleClientset(), fake.NewSimpleClientset()
	clusters := []discovery.TenantCluster{{Name: "a", Namespace: "vc-a", Source: "statefulset"}}
	reachable := map[string]bool{"a": true, "b": true}
	e := New(host, Options{Mode: "enforce", Now: time.Now,
		Clusters: func(string) []discovery.TenantCluster { return clusters },
		TenantClient: func(_ context.Context, cl discovery.TenantCluster) (kubernetes.Interface, error) {
			if !reachable[cl.Name] {
				return nil, errors.New("unreachable")
			}
			if cl.Name == "a" {
				return insideA, nil
			}
			return insideB, nil
		},
	})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "stripe", "invoice.payment_failed"); err != nil {
		t.Fatal(err)
	}
	has := func(c kubernetes.Interface) bool {
		_, err := c.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
		return err == nil
	}
	// Cluster b is created while acme is suspended.
	clusters = append(clusters, discovery.TenantCluster{Name: "b", Namespace: "vc-b", Source: "statefulset"})
	e.Resync(ctx)
	if !has(insideB) {
		t.Fatal("a cluster created during the suspension must get the policy at resync")
	}
	// The payment arrives while cluster b is unreachable.
	reachable["b"] = false
	_ = e.Set(ctx, "acme", Active, "invoice paid", "stripe", "invoice.paid")
	if has(insideA) || !has(insideB) {
		t.Fatalf("after payment: a=%v b=%v", has(insideA), has(insideB))
	}
	reachable["b"] = true
	e.Resync(ctx)
	if has(insideB) {
		t.Fatal("a cluster that missed the payment must be unblocked at resync")
	}
}
