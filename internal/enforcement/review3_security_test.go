package enforcement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// Third review: the verified pause against a tenant that is cluster admin
// inside its own cluster.

// The drain empties the cluster's own node. As vBilling scales the control
// plane down, the tenant (watching its nodes for the pause taint, from
// anywhere it holds a kubeconfig) starts an 8-GPU pod in kube-system, which
// the in-cluster policy exempts, tolerating every taint, and deletes the
// binding that lets vBilling read pods inside its cluster. confirmStopped
// takes the Forbidden answer for "the API went away with the control plane",
// so the pause is verified and the node stops billing while the pod runs on
// after the control plane is gone. PAUSE_KEEP_NAMESPACES=none, which the docs
// give untrusted tenants, changes nothing here.
func TestReview3RevokedReadAccessVerifiesAPauseWhileAPodRuns(t *testing.T) {
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	var mu sync.Mutex
	revoked := false
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			miner := podOn("miner", "kube-system", "gpu-1", corev1.Toleration{Operator: corev1.TolerationOpExists})
			miner.Spec.Containers = []corev1.Container{{Name: "m", Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}}}
			_ = inside.Tracker().Add(miner)
			mu.Lock()
			revoked = true
			mu.Unlock()
		}
		return false, nil, nil
	})
	inside.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if revoked {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "",
				errors.New(`clusterrolebinding "vbilling-enforcer" not found`))
		}
		return false, nil, nil
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans, KeepNamespaces: []string{},
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if _, err := inside.Tracker().Get(podsGVR, "kube-system", "miner"); err != nil {
		t.Fatalf("test setup: the tenant's pod is gone: %v", err)
	}
	if _, open := spans.Current(cl.ExternalID()); open {
		t.Fatalf("pause verified (capacity no longer billed) while kube-system/miner with 8 GPUs runs on the cluster's own node gpu-1: "+
			"a Forbidden pod list was taken as the API going away (status %+v)", e.Pauses())
	}
}

// v0.3.0 opened a pause span at the mark, before (and whatever) its drain
// left running, with By "vbilling". v0.3.1 reads every such span as
// verified and keeps it: after the upgrade, a paused cluster whose own nodes
// v0.3.1 itself could never verify (its API is down, metering knows the
// cluster has own nodes) stays unbilled. The same cluster paused by v0.3.1
// after a restart keeps billing (TestRestartBeforeVerificationKeepsBilling).
func TestReview3PauseSpanFromV030IsTakenAsVerifiedAfterUpgrade(t *testing.T) {
	ns := "vc-u"
	host := fake.NewSimpleClientset(controlPlane("u", ns, 0, map[string]string{discovery.AnnotationPausedBy: pause.ByVBilling,
		discovery.AnnotationPausedReplicas: "1"}), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	cl := discovery.TenantCluster{Name: "u", Namespace: ns, Source: "statefulset"}
	spans := pause.Memory()
	// pause-spans.json as v0.3.0 left it: opened at its mark.
	if _, err := spans.Begin(cl.ExternalID(), time.Now().UTC().Add(-6*time.Hour), pause.ByVBilling); err != nil {
		t.Fatal(err)
	}
	e := New(host, Options{Mode: "enforce", State: kvWith(t, "acme", Paused), DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		Freeze:       func(context.Context, discovery.TenantCluster) error { return nil },
		OwnNodes:     func(discovery.TenantCluster) (bool, bool) { return true, true },
		TenantClient: clientFor(fake.NewSimpleClientset())})
	e.Resync(context.Background())
	e.Wait()
	if sp, open := spans.Current(cl.ExternalID()); open {
		t.Fatalf("a span v0.3.0 opened without verifying anything (%+v) keeps the cluster's own nodes unbilled after the upgrade; "+
			"v0.3.1 itself would not verify this pause (status %+v)", sp, e.Pauses())
	}
}

// A variation of the second review's "lesser signal lifts a block": the
// fix lets only positive signals lower the billing hold, but any positive
// signal clears it, whatever it is about. Dunning marked the subscription
// unpaid (its latest invoice, a large one, failed every retry), which
// suspends the tenant. The tenant pays a small older invoice on the hosted
// invoice page. Stripe sends invoice.paid for it; the subscription stays
// unpaid (only paying the latest invoice reactivates it), so no event ever
// suspends the tenant again: one small payment lifts the suspension for as
// long as the debt stays open. A one-off invoice, or an update to another
// active subscription of the same customer, does the same.
func TestReview3PaymentOfAnotherInvoiceLiftsASuspensionForAnUnpaidSubscription(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	send := func(body string) {
		t.Helper()
		if _, err := e.HandleStripe(context.Background(), []byte(body), stripe.SignatureHeader([]byte(body), "whsec_test", now)); err != nil {
			t.Fatal(err)
		}
	}
	created := now.Add(-time.Hour).Unix()
	send(fmt.Sprintf(`{"id":"evt_unpaid","type":"customer.subscription.updated","created":%d,"data":{"object":`+
		`{"object":"subscription","id":"sub_metered","customer":"cus_acme","status":"unpaid","latest_invoice":"in_large"}}}`, created))
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("test setup: unpaid did not suspend: %s", st.State)
	}
	send(fmt.Sprintf(`{"id":"evt_small_paid","type":"invoice.paid","created":%d,"data":{"object":`+
		`{"object":"invoice","id":"in_small_old","customer":"cus_acme","subscription":"sub_metered","status":"paid","amount_paid":500}}}`, created+60))
	if st := e.StateOf("acme"); st.State != Suspended || nsLabels(t, kube)[LabelSuspended] != "true" {
		t.Fatalf("paying invoice in_small_old lifted the suspension while subscription sub_metered is still unpaid "+
			"(its latest invoice in_large is open): state %s, namespace labels %v", st.State, nsLabels(t, kube))
	}
}
