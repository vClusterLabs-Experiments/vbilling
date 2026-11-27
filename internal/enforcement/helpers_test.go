package enforcement

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// logLines is what vBilling logged during a test.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// count returns how many logged lines contain every one of parts.
func (l *logLines) count(parts ...string) int {
	n := 0
	for _, line := range strings.Split(l.String(), "\n") {
		all := line != ""
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

// captureLog collects the standard logger's lines until the test ends.
func captureLog(t *testing.T) *logLines {
	t.Helper()
	l := &logLines{}
	prev := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(prev) })
	return l
}

// eventually waits until ok holds, or fails the test with what.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

// ownedPod is a pod on a node that a workload controller created, and
// creates again when it is deleted.
func ownedPod(name, ns, node, kind, owner string) *corev1.Pod {
	p := podOn(name, ns, node)
	controller := true
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: owner, UID: types.UID(kind + "-" + owner), Controller: &controller}}
	return p
}

func hasPolicy(t *testing.T, kube kubernetes.Interface, name string) bool {
	t.Helper()
	_, err := kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("read admission policy %s: %v", name, err)
	}
	return err == nil
}

// storedPolicy reads a policy from what a cluster stores, whether or not
// its API answers.
func storedPolicy(inside *fake.Clientset, name string) bool {
	_, err := inside.Tracker().Get(admissionv1.SchemeGroupVersion.WithResource("validatingadmissionpolicies"), "", name)
	return err == nil
}

// storedPods counts the pods a cluster stores in a namespace, whether or
// not its API answers.
func storedPods(t *testing.T, inside *fake.Clientset, ns string) int {
	t.Helper()
	obj, err := inside.Tracker().List(corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithKind("Pod"), ns)
	if err != nil {
		t.Fatal(err)
	}
	return len(obj.(*corev1.PodList).Items)
}

func tainted(t *testing.T, kube kubernetes.Interface, node string) bool {
	t.Helper()
	n, err := kube.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return hasTaint(n, TaintPaused)
}

// source returns a health source by name.
func source(sources *health.Registry, name string) (health.Source, bool) {
	for _, s := range sources.List() {
		if s.Name == name {
			return s, true
		}
	}
	return health.Source{}, false
}

// quietHealth is a source registry that does not log.
func quietHealth() *health.Registry {
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	return h
}
