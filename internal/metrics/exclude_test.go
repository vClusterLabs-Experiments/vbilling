package metrics

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func excludedNS(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelExclude: "true"}}}
}

// In usage mode, a namespace labeled vbilling.vcluster.com/exclude=true
// inside a cluster is skipped like kube-system, also when the cluster is
// paused and metered from its saved inventory. A cluster whose reader role
// may not list namespaces has none excluded: all its pods bill.
func TestExcludedNamespacesInsideAClusterInUsageMode(t *testing.T) {
	gpuNode := node("private-gpu", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	operator := pod("nvidia-driver", "private-gpu", withRequests("4", "16Gi"), inNamespace("gpu-operator"))
	trainer := pod("trainer", "private-gpu", withRequests("2", "8Gi"), inNamespace("research"))
	cpu := func(evs []usage.Event) float64 { got, _ := find(evs, usage.MetricCPUCoreHours); return got }

	conn := tenantConn(gpuNode, operator, trainer, excludedNS("gpu-operator"))
	c := pausedCollector(t, pause.DefaultKeep, nil)
	c.opts.PrivateNodeBilling = "usage"
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	tg := target("gpu", true)
	out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got := cpu(out[we]); !near(got, usage.Round(2.0/60)) {
		t.Fatalf("CPU core-hours %v, want the research pod's 2 cores only", got)
	}

	// Paused by someone else: metered from the inventory last read.
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	tg.Cluster.Paused, tg.Cluster.Ready = true, false
	out, _, err = c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got := cpu(out[we]); !near(got, usage.Round(2.0/60)) {
		t.Fatalf("paused: CPU core-hours %v, want the research pod's 2 cores only", got)
	}

	refusing := tenantConn(gpuNode, operator, trainer, excludedNS("gpu-operator"))
	refusing.Kube.(*fake.Clientset).PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("vbilling-reader"))
	})
	out, st := collectWith(t, Options{PrivateNodeBilling: "usage"}, nil, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": refusing}})
	})
	if len(st.TenantAPIFailed) != 0 {
		t.Fatalf("a refused namespace list failed the cluster's read: %v", st.TenantAPIFailed)
	}
	if got := cpu(out[we]); !near(got, usage.Round(6.0/60)) {
		t.Fatalf("namespaces not readable: CPU core-hours %v, want both pods' 6 cores", got)
	}
}
