package enforcement

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// transportErr is a call that never reached the API, the way client-go
// reports it.
func transportErr(err error) error {
	return &url.Error{Op: "Get", URL: "https://private.tenant:443/api/v1/pods", Err: err}
}

// stopsWithItsControlPlane makes a cluster's API behave as a vCluster's
// does when its control plane is scaled to zero: it answers one more check
// of the cluster's own nodes (whose last read is the pause policy's
// binding), then refuses connections until the control plane is scaled up
// again. A pause is verified only once its API was seen to go away.
func stopsWithItsControlPlane(host, inside *fake.Clientset) {
	var mu sync.Mutex
	stopping, gone := false, false
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		patch := string(a.(ktesting.PatchActionImpl).Patch)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(patch, `"replicas":0`):
			stopping = true
		case strings.Contains(patch, `"replicas":`):
			stopping, gone = false, false
		}
		return false, nil, nil
	})
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	inside.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if gone {
			return true, nil, transportErr(refused)
		}
		if stopping && a.GetVerb() == "get" && a.GetResource().Resource == "validatingadmissionpolicybindings" {
			gone = true // its last answer
		}
		return false, nil, nil
	})
}

// Once the control plane is scaled down, the checks of the cluster's own
// nodes end only when its API goes away with it: the connection is refused
// or reset, ends early, or the proxy in front of it answers 503. Any other
// failure is the API refusing to answer (a tenant can revoke vBilling's
// access) or not answering: the pause stays unverified and keeps billing.
func TestOnlyTheAPIGoingAwayEndsThePauseChecks(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	pods := schema.GroupResource{Resource: "pods"}
	for _, tc := range []struct {
		name     string
		err      error
		verified bool
	}{
		{"connection refused", transportErr(refused), true},
		{"connection reset", transportErr(reset), true},
		{"EOF", transportErr(io.EOF), true},
		{"unexpected EOF", transportErr(io.ErrUnexpectedEOF), true},
		{"503 from the proxy", apierrors.NewServiceUnavailable("the vCluster is not ready"), true},
		{"refused, as text only", errors.New(`Get "https://10.0.0.9/api/v1/pods": dial tcp 10.0.0.9:443: connect: connection refused`), true},
		{"forbidden", apierrors.NewForbidden(pods, "", errors.New("RBAC: access denied")), false},
		{"unauthorized", apierrors.NewUnauthorized("token revoked"), false},
		{"timeout", transportErr(context.DeadlineExceeded), false},
		{"server timeout", apierrors.NewTimeoutError("list pods", 1), false},
		{"internal error", apierrors.NewInternalError(errors.New("etcd unavailable")), false},
		{"not found", apierrors.NewNotFound(pods, ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
			var mu sync.Mutex
			stopping := false
			host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
				if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
					mu.Lock()
					stopping = true
					mu.Unlock()
				}
				return false, nil, nil
			})
			fail := func(ktesting.Action) (bool, runtime.Object, error) {
				mu.Lock()
				defer mu.Unlock()
				if stopping {
					return true, nil, tc.err
				}
				return false, nil, nil
			}
			inside.PrependReactor("list", "pods", fail)
			inside.PrependReactor("list", "nodes", fail)
			spans := pause.Memory()
			e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
				Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
			if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
				t.Fatal(err)
			}
			e.Wait()
			_, open := spans.Current(cl.ExternalID())
			st := onlyStatus(t, e)
			if open != tc.verified || st.Verified != tc.verified {
				t.Fatalf("span open %t, status %+v; want verified %t", open, st, tc.verified)
			}
			if !tc.verified && !strings.Contains(st.Unverified, "could not be checked") {
				t.Fatalf("status %+v does not say why", st)
			}
		})
	}
}

// A pause installs vbilling-paused before it drains anything: every new pod
// is denied, in every namespace, kube-system included, except the
// kubelet's mirror pods. Resume removes it.
func TestPauseDeniesEveryNewPodUntilResume(t *testing.T) {
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	var order pauseLog
	inside.PrependReactor("create", "validatingadmissionpolicies", func(a ktesting.Action) (bool, runtime.Object, error) {
		order.add("create policy " + a.(ktesting.CreateActionImpl).Object.(*admissionv1.ValidatingAdmissionPolicy).Name)
		return false, nil, nil
	})
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		order.add("delete pod")
		return false, nil, nil
	})
	stopsWithItsControlPlane(host, inside)
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery = 10 * time.Millisecond
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := order.list(); len(got) < 2 || got[0] != "create policy "+PausedPolicyName {
		t.Fatalf("order = %v, want the pause policy created before the first pod is deleted", got)
	}
	// The cluster's API is down while it is paused: read what it stores.
	po, err := inside.Tracker().Get(admissionv1.SchemeGroupVersion.WithResource("validatingadmissionpolicies"), "", PausedPolicyName)
	if err != nil {
		t.Fatal(err)
	}
	bo, err := inside.Tracker().Get(admissionv1.SchemeGroupVersion.WithResource("validatingadmissionpolicybindings"), "", PausedPolicyName)
	if err != nil {
		t.Fatal(err)
	}
	p, b := po.(*admissionv1.ValidatingAdmissionPolicy), bo.(*admissionv1.ValidatingAdmissionPolicyBinding)
	api := inside.AdmissionregistrationV1()
	if p.Spec.MatchConstraints.NamespaceSelector != nil || len(p.Spec.Validations) != 2 ||
		p.Spec.Validations[0].Expression != createdMirror || p.Spec.Validations[1].Expression != gatesKept ||
		!coversPods(p.Spec.MatchConstraints.ResourceRules, admissionv1.Update) ||
		len(b.Spec.ValidationActions) != 1 || b.Spec.ValidationActions[0] != admissionv1.Deny {
		t.Fatalf("pause policy %+v, binding %+v: want every namespace, mirror pods only, no gate removed, denied", p.Spec, b.Spec)
	}
	if why := policyNarrowed(p.Spec, b.Spec); why != "" {
		t.Fatalf("vBilling's own pause policy reads as changed: %s", why)
	}
	if _, open := spans.Current(cl.ExternalID()); !open {
		t.Fatalf("pause not verified: %+v", e.Pauses())
	}
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if _, err := api.ValidatingAdmissionPolicies().Get(ctx, PausedPolicyName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pause policy after resume: %v", err)
	}
	if _, err := api.ValidatingAdmissionPolicyBindings().Get(ctx, PausedPolicyName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pause policy binding after resume: %v", err)
	}
}

// A tenant that removes or disarms the pause policy while the control plane
// stops could start a pod after the last check: the pause stays
// unverified.
func TestPausePolicyRemovedWhileTheControlPlaneStopsLeavesThePauseUnverified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(ctx context.Context, api admissionTampering) error
		why    string
	}{
		{"binding deleted", func(ctx context.Context, api admissionTampering) error {
			return api.bindings.Delete(ctx, PausedPolicyName, metav1.DeleteOptions{})
		}, "binding of the pause admission policy"},
		{"audit only", func(ctx context.Context, api admissionTampering) error {
			b, err := api.bindings.Get(ctx, PausedPolicyName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			b.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Audit}
			_, err = api.bindings.Update(ctx, b, metav1.UpdateOptions{})
			return err
		}, "does not deny"},
		{"kube-system exempt", func(ctx context.Context, api admissionTampering) error {
			p, err := api.policies.Get(ctx, PausedPolicyName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			p.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"}}}}
			_, err = api.policies.Update(ctx, p, metav1.UpdateOptions{})
			return err
		}, "no longer matches every new pod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
			api := admissionTampering{policies: inside.AdmissionregistrationV1().ValidatingAdmissionPolicies(),
				bindings: inside.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings()}
			host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
				if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
					if err := tc.tamper(context.Background(), api); err != nil {
						t.Errorf("tamper: %v", err)
					}
				}
				return false, nil, nil
			})
			spans := pause.Memory()
			e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
				Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
			if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
				t.Fatal(err)
			}
			e.Wait()
			if _, open := spans.Current(cl.ExternalID()); open {
				t.Fatalf("pause verified although the pause policy was disarmed: %+v", e.Pauses())
			}
			if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, tc.why) {
				t.Fatalf("status = %+v, want it unverified, saying %q", st, tc.why)
			}
		})
	}
}

type admissionTampering struct {
	policies interface {
		Get(context.Context, string, metav1.GetOptions) (*admissionv1.ValidatingAdmissionPolicy, error)
		Update(context.Context, *admissionv1.ValidatingAdmissionPolicy, metav1.UpdateOptions) (*admissionv1.ValidatingAdmissionPolicy, error)
	}
	bindings interface {
		Get(context.Context, string, metav1.GetOptions) (*admissionv1.ValidatingAdmissionPolicyBinding, error)
		Update(context.Context, *admissionv1.ValidatingAdmissionPolicyBinding, metav1.UpdateOptions) (*admissionv1.ValidatingAdmissionPolicyBinding, error)
		Delete(context.Context, string, metav1.DeleteOptions) error
	}
}

// A pause policy the cluster's API refuses to install is a failed drain:
// the control plane stays up and the pause is retried.
func TestPausePolicyThatCannotBeInstalledKeepsTheControlPlaneUp(t *testing.T) {
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	inside.PrependReactor("create", "validatingadmissionpolicies", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"},
			PausedPolicyName, errors.New("RBAC: access denied"))
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: time.Minute, Pauses: spans, Now: newClock().Now,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 1 {
		t.Fatalf("control plane replicas = %d, want it kept up while the pause policy cannot be installed", r)
	}
	if st := onlyStatus(t, e); !strings.Contains(st.Error, PausedPolicyName) {
		t.Fatalf("status = %+v, want the failed policy install", st)
	}
	if !podNames(t, inside, "default")["inference"] {
		t.Fatal("drained without the pause policy in place")
	}
}

// Credentials that may not touch vbilling-paused (the in-cluster role
// documented before it existed) never let a pause install it, so a resume
// with them lifts the taint all the same: a paying tenant's nodes must
// never stay tainted.
func TestResumeWithoutAccessToThePausePolicyLiftsTheTaint(t *testing.T) {
	cl, host, inside := privateCluster()
	ctx := context.Background()
	node, _ := inside.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	node.Spec.Taints = []corev1.Taint{pausedTaint}
	if _, err := inside.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	forbidden := func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.(ktesting.DeleteActionImpl).Name == PausedPolicyName {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: a.GetResource().Resource},
				PausedPolicyName, errors.New("RBAC: access denied"))
		}
		return false, nil, nil
	}
	inside.PrependReactor("delete", "validatingadmissionpolicies", forbidden)
	inside.PrependReactor("delete", "validatingadmissionpolicybindings", forbidden)
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	node, _ = inside.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if hasTaint(node, TaintPaused) {
		t.Fatal("the pause taint stayed on a paying tenant's node")
	}
}

// An external cluster's API stays up, so a cluster admin inside can remove
// the pause policy, run something and put the policy back between two
// passes. A pass that finds the policy removed or changed ends the span at
// the last pass that found it in place, like a pass that finds workloads.
func TestExternalPauseSpanEndsWhenThePolicyWasRemoved(t *testing.T) {
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-1"}})
	cl := discovery.TenantCluster{Name: "edge", External: true}
	clock := newClock()
	start := clock.Now()
	spans := pause.Memory()
	e := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	clock.add(15 * time.Second)
	e.Guard(ctx) // the policy is in place: the span holds
	e.Wait()
	clock.add(15 * time.Second)
	if err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(ctx, PausedPolicyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Guard(ctx)
	e.Wait()
	got := spans.Spans(cl.ExternalID())
	if len(got) != 2 || !got[0].From.Equal(start) || !got[0].To.Equal(start.Add(15*time.Second)) || !got[1].Open() || !got[1].From.Equal(start.Add(30*time.Second)) {
		t.Fatalf("spans = %+v, want the first ending at the last pass that found the policy and a new one from now", got)
	}
	if _, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PausedPolicyName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the pass did not put the pause policy back: %v", err)
	}
}

// The API server fills in defaults the comparison must not take for a
// change; anything that lets a pod through, or lets a pod's scheduling
// gates go, must count as one.
func TestPausePolicyComparison(t *testing.T) {
	equivalent := admissionv1.Equivalent
	all := admissionv1.AllScopes
	fail := admissionv1.Fail
	ignore := admissionv1.Ignore
	stored := func() (admissionv1.ValidatingAdmissionPolicySpec, admissionv1.ValidatingAdmissionPolicyBindingSpec) {
		return admissionv1.ValidatingAdmissionPolicySpec{
				FailurePolicy: &fail,
				MatchConstraints: &admissionv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{}, ObjectSelector: &metav1.LabelSelector{}, MatchPolicy: &equivalent,
					ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
						Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
						Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}, Scope: &all},
					}}},
				},
				Validations: []admissionv1.Validation{{Expression: createdMirror}, {Expression: gatesKept}},
			}, admissionv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName: PausedPolicyName, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
				MatchResources: &admissionv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{}, ObjectSelector: &metav1.LabelSelector{}, MatchPolicy: &equivalent},
			}
	}
	if p, b := stored(); policyNarrowed(p, b) != "" {
		t.Fatalf("defaults read as a change: %s", policyNarrowed(p, b))
	}
	for name, change := range map[string]func(*admissionv1.ValidatingAdmissionPolicySpec, *admissionv1.ValidatingAdmissionPolicyBindingSpec){
		"ignore failures": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.FailurePolicy = &ignore
		},
		"match condition": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConditions = []admissionv1.MatchCondition{{Name: "x", Expression: "false"}}
		},
		"object selector": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConstraints.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}
		},
		"excluded pods": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConstraints.ExcludeResourceRules = p.MatchConstraints.ResourceRules
		},
		"updates only": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConstraints.ResourceRules[0].Operations = []admissionv1.OperationType{admissionv1.Update}
		},
		"creations only": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConstraints.ResourceRules[0].Operations = []admissionv1.OperationType{admissionv1.Create}
		},
		"gates may go": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.Validations[1].Expression = "true"
		},
		"v0.3.1's first policy": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConstraints.ResourceRules[0].Operations = []admissionv1.OperationType{admissionv1.Create}
			p.Validations = []admissionv1.Validation{{Expression: mirrorPod}}
		},
		"one pod by name": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.MatchConstraints.ResourceRules[0].ResourceNames = []string{"x"}
		},
		"lets everything through": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.Validations[0].Expression = "true"
		},
		"a second validation": func(p *admissionv1.ValidatingAdmissionPolicySpec, _ *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			p.Validations = append(p.Validations, admissionv1.Validation{Expression: "true"})
		},
		"warn only": func(_ *admissionv1.ValidatingAdmissionPolicySpec, b *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			b.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
		},
		"another policy": func(_ *admissionv1.ValidatingAdmissionPolicySpec, b *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			b.PolicyName = "other"
		},
		"binding narrowed": func(_ *admissionv1.ValidatingAdmissionPolicySpec, b *admissionv1.ValidatingAdmissionPolicyBindingSpec) {
			b.MatchResources.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
		},
	} {
		p, b := stored()
		change(&p, &b)
		if policyNarrowed(p, b) == "" {
			t.Errorf("%s: not seen as a change", name)
		}
	}
}
