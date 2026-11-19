package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// Fixtures: testdata/platform-4.9.0 was captured from a live vCluster
// Platform 4.9.0 (no Tenant resource) with a least-privilege access key;
// testdata/platform-4.12.7 and platform-5.0.0-rc.8 are generated from the
// loft-sh/api Go types by testdata/gen.

const testKey = "0123456789abcdef0123456789abcdef0123"

// fakePlatform serves a fixture directory the way the Platform does.
type fakePlatform struct {
	t    *testing.T
	dir  string
	srv  *httptest.Server
	mu   sync.Mutex
	down bool                           // answer 503 to everything
	keep func(item map[string]any) bool // filters virtualclusterinstances
	hits map[string]int                 // by resource
	seen []*http.Request                // proxy requests
	edit func(kind string, items []any) []any
}

func newFakePlatform(t *testing.T, version string) *fakePlatform {
	f := &fakePlatform{t: t, dir: filepath.Join("testdata", version), hits: map[string]int{}}
	f.srv = httptest.NewTLSServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlatform) setDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }

func (f *fakePlatform) setFilter(keep func(map[string]any) bool) {
	f.mu.Lock()
	f.keep = keep
	f.mu.Unlock()
}

func (f *fakePlatform) count(res string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits[res] }

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down, keep := f.down, f.keep
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		http.Error(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`, http.StatusUnauthorized)
		return
	}
	if down {
		http.Error(w, "upstream connect error", http.StatusServiceUnavailable)
		return
	}
	const mgmt = "/kubernetes/management/apis/management.loft.sh/v1"
	w.Header().Set("Content-Type", "application/json")
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/kubernetes/project/"):
		f.mu.Lock()
		f.seen = append(f.seen, r)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[{"metadata":{"name":"gpu-node-1"}}]}`))
	case p == mgmt:
		f.file(w, "discovery.json", nil)
	case p == mgmt+"/selves" && r.Method == http.MethodPost:
		f.file(w, "self.json", nil)
	case strings.HasPrefix(p, mgmt+"/"):
		res := strings.TrimPrefix(p, mgmt+"/")
		f.mu.Lock()
		f.hits[res]++
		f.mu.Unlock()
		if res == "virtualclusterinstances" {
			f.file(w, res+".json", keep)
			return
		}
		f.file(w, res+".json", nil)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePlatform) file(w http.ResponseWriter, name string, keep func(map[string]any) bool) {
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		return
	}
	if err != nil {
		f.t.Errorf("fixture %s: %v", name, err)
		return
	}
	if keep != nil {
		var l map[string]any
		_ = json.Unmarshal(b, &l)
		var items []any
		for _, it := range l["items"].([]any) {
			if keep(it.(map[string]any)) {
				items = append(items, it)
			}
		}
		l["items"] = items
		b, _ = json.Marshal(l)
	}
	_, _ = w.Write(b)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration)    { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newClock(t time.Time) *clock       { return &clock{t: t} }
func uidOf(item map[string]any) string  { return item["metadata"].(map[string]any)["uid"].(string) }
func nameOf(item map[string]any) string { return item["metadata"].(map[string]any)["name"].(string) }

func newResolver(t *testing.T, f *fakePlatform, cluster, state string, clk *clock) (*Resolver, *health.Registry) {
	t.Helper()
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	h.SetClock(clk.now)
	r, err := New(Options{
		Client:    ClientOptions{URL: f.srv.URL, AccessKey: testKey, Insecure: true, UserAgent: "vbilling/test"},
		Cluster:   cluster,
		Refresh:   time.Minute,
		StatePath: state,
		Health:    h,
		Now:       clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, h
}

func workload(ns, name string, created time.Time, labels map[string]string) discovery.TenantCluster {
	return discovery.TenantCluster{Name: name, Namespace: ns, UID: "uid-sts-" + name, CreatedAt: created, Ready: true,
		Source: "statefulset", Labels: labels, Annotations: map[string]string{}}
}

func bind(t *testing.T, r *Resolver, cls ...discovery.TenantCluster) map[string]*discovery.TenantCluster {
	t.Helper()
	out, err := r.Bind(context.Background(), cls)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*discovery.TenantCluster{}
	for i := range out {
		m[out[i].Name] = &out[i]
	}
	return m
}

func status(h *health.Registry, name string) health.Status {
	for _, s := range h.List() {
		if s.Name == name {
			return s.Status
		}
	}
	return ""
}

var old = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestPlatform49ProjectsAreTheAccounts(t *testing.T) {
	f := newFakePlatform(t, "platform-4.9.0")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, h := newResolver(t, f, "loft-cluster", "", clk)
	got := bind(t, r,
		workload("loft-acme-ai-v-acme-train", "acme-train", old, nil),
		workload("loft-globex-v-globex-infer", "globex-infer", old, nil),
		workload("team-x", "helm-installed", old, nil),
	)
	acme := got["acme-train"]
	if acme.Platform == nil || acme.TenantID(discovery.TenantPerPlatform) != "project-acme-ai" || acme.Project != "acme-ai" ||
		acme.Meta("display-name") != "Acme AI Training" || acme.DisplayName != "Acme training cluster" ||
		acme.Platform.ProjectUID != "f384d739-3c2b-465d-8fc2-6fdb5095233c" || acme.Platform.InstanceUID != "e3ba5b9a-41e5-412f-bbae-540798dd28a3" {
		t.Fatalf("acme-train: %+v / %+v", acme, acme.Platform)
	}
	if g := got["globex-infer"]; g.TenantID(discovery.TenantPerPlatform) != "project-globex" || g.Meta("display-name") != "Globex Research" {
		t.Fatalf("globex-infer: %+v", g)
	}
	if o := got["helm-installed"]; o.Platform != nil || o.TenantID(discovery.TenantPerPlatform) != "vcluster-team-x-helm-installed" {
		t.Fatalf("a cluster without an instance falls back to the label rules: %+v", o)
	}
	if f.count("tenants") != 0 {
		t.Errorf("tenants listed %d times on a Platform whose discovery has no Tenant resource", f.count("tenants"))
	}
	if status(h, "platform") != health.OK || status(h, "platform/vcluster-loft-acme-ai-v-acme-train-acme-train") != health.OK ||
		status(h, "platform/vcluster-team-x-helm-installed") != health.Empty {
		t.Errorf("health: %+v", h.List())
	}
	md := acme.PlatformMetadata()
	if md["platform_account"] != "project" || md["platform_project_uid"] != "f384d739-3c2b-465d-8fc2-6fdb5095233c" {
		t.Errorf("metadata: %v", md)
	}
}

func TestPlatform412TenantOwnsProjects(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, h := newResolver(t, f, "cp-1", "", clk)
	// Labels on the workload and its namespace are set by the people who
	// create clusters (vcluster.yaml): they must not choose the payer or price.
	train := workload("vc-train-1", "train-1", old, map[string]string{
		discovery.AnnotationPrefix + "tenant": "globex", discovery.AnnotationPrefix + "plan": "free",
		discovery.AnnotationPrefix + "exclude": "true", discovery.AnnotationPrefix + "prometheus-url": "http://prom.vc-train-1:9090",
	})
	got := bind(t, r, train,
		workload("vc-infer-1", "infer-1", old, nil),
		workload("vc-lab", "lab", old, nil),
		workload("vc-ghost", "ghost", old, nil),
	)
	tr := got["train-1"]
	if tr.TenantID(discovery.TenantPerPlatform) != "tenant-acme" || tr.Meta("plan") != "enterprise" || tr.Meta("email") != "billing@acme.example" ||
		tr.Meta("display-name") != "Acme AI" || tr.Meta("exclude") != "" || tr.Project != "acme-train" || tr.Platform.TenantUID != "uid-tenant-acme" {
		t.Fatalf("train-1: tenant %s plan %q email %q name %q exclude %q project %s binding %+v",
			tr.TenantID(discovery.TenantPerPlatform), tr.Meta("plan"), tr.Meta("email"), tr.Meta("display-name"), tr.Meta("exclude"), tr.Project, tr.Platform)
	}
	if tr.Meta("prometheus-url") != "" {
		t.Errorf("a metering setting on the workload must not survive the binding: %q", tr.Meta("prometheus-url"))
	}
	if in := got["infer-1"]; in.TenantID(discovery.TenantPerPlatform) != "tenant-acme" || in.Platform.InstancePhase != "Sleeping" {
		t.Errorf("infer-1: %+v", in.Platform)
	}
	if lab := got["lab"]; lab.TenantID(discovery.TenantPerPlatform) != "research-legacy" || lab.Platform.Tenant != "" {
		t.Errorf("an explicit billing ID on the project keeps the existing customer: %s", lab.TenantID(discovery.TenantPerPlatform))
	}
	if g := got["ghost"]; g.TenantID(discovery.TenantPerPlatform) != "tenant-initech" || status(h, "platform/vcluster-vc-ghost-ghost") != health.Failing {
		t.Errorf("a project naming a missing Tenant is billed to that Tenant and flagged: %s %v", g.TenantID(discovery.TenantPerPlatform), h.List())
	}
	md := tr.PlatformMetadata()
	if md["platform_account"] != "tenant" || md["platform_tenant"] != "acme" || md["platform_tenant_uid"] != "uid-tenant-acme" {
		t.Errorf("metadata: %v", md)
	}
	props := tr.PlatformProperties()
	if props["platform_instance_uid"] != "uid-vci-train-1" || props["platform_project_uid"] != "uid-project-acme-train" || props["platform_tenant_uid"] != "uid-tenant-acme" {
		t.Errorf("properties: %v", props)
	}
	if _, _, err := r.ProxyConfig(*got["infer-1"]); err == nil || !strings.Contains(err.Error(), "sleeping") {
		t.Errorf("a sleeping instance must not be woken for metering: %v", err)
	}
}

func TestClusterNameSeparatesConnectedClusters(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	// Without PLATFORM_CLUSTER, an instance on cp-2 with the same namespace
	// and name also points at vc-train-1/train-1: ambiguous, never guessed.
	r, h := newResolver(t, f, "", "", clk)
	got := bind(t, r, workload("vc-train-1", "train-1", old, nil))
	if got["train-1"].Platform != nil || status(h, "platform/vcluster-vc-train-1-train-1") != health.Failing {
		t.Fatalf("ambiguous instances must not bind: %+v %v", got["train-1"].Platform, h.List())
	}
}

func TestPlatform5OwnerAndExclusiveLabels(t *testing.T) {
	f := newFakePlatform(t, "platform-5.0.0-rc.8")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, h := newResolver(t, f, "cp-1", "", clk)
	got := bind(t, r,
		workload("vc-train-1", "train-1", old, nil),
		workload("vc-gpu", "gpu", old, nil),
		workload("vc-both", "both", old, nil),
	)
	if id := got["train-1"].TenantID(discovery.TenantPerPlatform); id != "tenant-acme" {
		t.Errorf("tenant.vcluster.com/owner: %s", id)
	}
	if id := got["gpu"].TenantID(discovery.TenantPerPlatform); id != "tenant-globex" {
		t.Errorf("tenant.vcluster.com/exclusive-to: %s", id)
	}
	if id := got["both"].TenantID(discovery.TenantPerPlatform); id != "project-conflict" || status(h, "platform/vcluster-vc-both-both") != health.Failing {
		t.Errorf("both labels: owned by no Tenant and flagged, got %s", id)
	}
}

func TestMoveBetweenProjectsKeepsHistory(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	f.setFilter(func(it map[string]any) bool { return uidOf(it) != "uid-vci-mover-new" })
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC) // instances' creation in the fixture
	clk := newClock(t0.Add(24 * time.Hour))
	state := filepath.Join(t.TempDir(), "platform-bindings.json")
	r, _ := newResolver(t, f, "cp-1", state, clk)
	mover := workload("vc-mover", "mover", t0, nil)
	if id := bind(t, r, mover)["mover"].TenantID(discovery.TenantPerPlatform); id != "tenant-acme" {
		t.Fatalf("before the move: %s", id)
	}

	// The Platform clones the instance into project research (created at
	// t0+48h) and marks the old one; vBilling sees it a day later.
	f.setFilter(nil)
	clk.add(48 * time.Hour)
	moved := bind(t, r, mover)["mover"]
	if id := moved.TenantID(discovery.TenantPerPlatform); id != "research-legacy" {
		t.Fatalf("after the move the new instance wins over the one marked ignoreReconciliation: %s", id)
	}
	// Windows metered late are billed to the owner of their time.
	before, ok := r.OwnerAt(*moved, t0.Add(47*time.Hour))
	if !ok || before.TenantID(discovery.TenantPerPlatform) != "tenant-acme" || before.Project != "acme-train" || before.Meta("plan") != "enterprise" {
		t.Errorf("window before the move: %v %s %s", ok, before.TenantID(discovery.TenantPerPlatform), before.Project)
	}
	if edge, ok := r.OwnerAt(*moved, t0.Add(48*time.Hour)); !ok || edge.TenantID(discovery.TenantPerPlatform) != "tenant-acme" {
		t.Errorf("the window ending at the move belongs to the old owner: %v %s", ok, edge.TenantID(discovery.TenantPerPlatform))
	}
	if _, ok := r.OwnerAt(*moved, t0.Add(49*time.Hour)); ok {
		t.Errorf("a window after the move uses the current owner")
	}

	// Live, a move is seen at the next read: windows the ledger already
	// holds stay with the old owner, and the history agrees with them.
	f.setFilter(func(it map[string]any) bool { return uidOf(it) != "uid-vci-mover-new" })
	live, _ := newResolver(t, f, "cp-1", "", clk)
	bind(t, live, mover)
	committed := t0.Add(48*time.Hour + 2*time.Minute) // after the new instance's creation
	live.opts.Committed = func() (time.Time, bool) { return committed, true }
	f.setFilter(nil)
	clk.add(time.Minute)
	seen := bind(t, live, mover)["mover"]
	if c, ok := live.OwnerAt(*seen, committed); !ok || c.TenantID(discovery.TenantPerPlatform) != "tenant-acme" {
		t.Errorf("a window already in the ledger must keep its owner in the history: %v %s", ok, c.TenantID(discovery.TenantPerPlatform))
	}
	if _, ok := live.OwnerAt(*seen, committed.Add(time.Minute)); ok {
		t.Errorf("the first window after the ledger goes to the new owner")
	}

	// The history survives a restart.
	r2, _ := newResolver(t, f, "cp-1", state, clk)
	again := bind(t, r2, mover)["mover"]
	if b, ok := r2.OwnerAt(*again, t0.Add(30*time.Hour)); !ok || b.TenantID(discovery.TenantPerPlatform) != "tenant-acme" {
		t.Errorf("history after restart: %v %s", ok, b.TenantID(discovery.TenantPerPlatform))
	}
}

func TestOutageNeverMovesUsage(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	state := filepath.Join(t.TempDir(), "platform-bindings.json")
	r, h := newResolver(t, f, "cp-1", state, clk)
	train := workload("vc-train-1", "train-1", old, nil)
	bind(t, r, train)

	f.setDown(true)
	clk.add(2 * time.Minute)
	brandNew := workload("vc-new", "new", clk.now(), nil) // created during the outage
	oldUnbound := workload("team-x", "helm", old, nil)
	got := bind(t, r, train, brandNew, oldUnbound)
	if id := got["train-1"].TenantID(discovery.TenantPerPlatform); id != "tenant-acme" {
		t.Errorf("a known cluster keeps its owner during an outage: %s", id)
	}
	if _, metered := got["new"]; metered {
		t.Errorf("a cluster created during an outage must wait for its owner, not be billed to a guess")
	}
	if o := got["helm"]; o.Platform != nil || o.TenantID(discovery.TenantPerPlatform) != "vcluster-team-x-helm" {
		t.Errorf("an old cluster without an instance keeps the label rules: %+v", o)
	}
	if status(h, "platform") != health.Failing || status(h, "platform/vcluster-vc-new-new") != health.Failing {
		t.Errorf("outage not reported: %v", h.List())
	}

	// A restart while the Platform is still down uses the persisted owners.
	r2, _ := newResolver(t, f, "cp-1", state, clk)
	if id := bind(t, r2, train)["train-1"].TenantID(discovery.TenantPerPlatform); id != "tenant-acme" {
		t.Errorf("after a restart during the outage: %s", id)
	}
	// With nothing persisted and no answer yet, nothing can be attributed.
	r3, _ := newResolver(t, f, "cp-1", filepath.Join(t.TempDir(), "x.json"), clk)
	if _, err := r3.Bind(context.Background(), []discovery.TenantCluster{train}); !errors.Is(err, ErrNotReady) {
		t.Errorf("first start during an outage: %v", err)
	}

	f.setDown(false)
	clk.add(time.Minute)
	if got := bind(t, r, train, brandNew); got["new"].Platform != nil || got["new"].TenantID(discovery.TenantPerPlatform) != "vcluster-vc-new-new" {
		t.Errorf("once the Platform answers, a cluster it does not know falls back: %+v", got["new"])
	}
}

func TestNewClusterReadsThePlatformAgain(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	f.setFilter(func(it map[string]any) bool { return nameOf(it) != "lab" })
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, _ := newResolver(t, f, "cp-1", "", clk)
	bind(t, r, workload("vc-train-1", "train-1", old, nil))
	reads := f.count("virtualclusterinstances")

	// The Platform created instance "lab" and its workload after the last
	// read; the refresh interval has not passed.
	f.setFilter(nil)
	clk.add(10 * time.Second)
	got := bind(t, r, workload("vc-train-1", "train-1", old, nil), workload("vc-lab", "lab", clk.now(), nil))
	if id := got["lab"].TenantID(discovery.TenantPerPlatform); id != "research-legacy" {
		t.Errorf("a new cluster must be bound from a fresh read, not billed to a fallback: %s", id)
	}
	if f.count("virtualclusterinstances") != reads+1 {
		t.Errorf("reads: %d, want one forced read", f.count("virtualclusterinstances")-reads)
	}
}

func TestOrphanedClusterKeepsOwnerUntilGrace(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, h := newResolver(t, f, "cp-1", "", clk)
	r.opts.OrphanGrace = time.Hour
	train := workload("vc-train-1", "train-1", old, nil)
	bind(t, r, train)

	f.setFilter(func(it map[string]any) bool { return nameOf(it) != "train-1" })
	clk.add(2 * time.Minute)
	if id := bind(t, r, train)["train-1"].TenantID(discovery.TenantPerPlatform); id != "tenant-acme" ||
		status(h, "platform/vcluster-vc-train-1-train-1") != health.Failing {
		t.Errorf("instance gone: keep the last owner and flag it, got %s", id)
	}
	clk.add(time.Hour)
	if c := bind(t, r, train)["train-1"]; c.Platform != nil || c.TenantID(discovery.TenantPerPlatform) != "vcluster-vc-train-1-train-1" {
		t.Errorf("after PLATFORM_ORPHAN_GRACE the label rules apply: %+v", c.Platform)
	}

	// A new workload under the same name is a different cluster.
	f.setFilter(nil)
	clk.add(time.Minute)
	bind(t, r, train)
	f.setFilter(func(it map[string]any) bool { return nameOf(it) != "train-1" })
	clk.add(time.Minute)
	reborn := train
	reborn.UID = "uid-sts-train-1-v2"
	if c := bind(t, r, reborn)["train-1"]; c.Platform != nil {
		t.Errorf("a new workload must not inherit the old one's owner: %+v", c.Platform)
	}
}

func TestProxyReachesClusterWithAccessKey(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, _ := newResolver(t, f, "cp-1", "", clk)
	tr := bind(t, r, workload("vc-train-1", "train-1", old, nil))["train-1"]
	cfg, version, err := r.ProxyConfig(*tr)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.srv.URL + "/kubernetes/project/acme-train/virtualcluster/train-1"; cfg.Host != want || !strings.HasPrefix(version, "platform:acme-train/train-1@uid-vci-train-1#") {
		t.Fatalf("proxy %s (%s), want %s", cfg.Host, version, want)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kube.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil || len(nodes.Items) != 1 {
		t.Fatalf("list nodes through the proxy: %v %v", nodes, err)
	}
	req := f.seen[len(f.seen)-1]
	if req.URL.Path != "/kubernetes/project/acme-train/virtualcluster/train-1/api/v1/nodes" || req.Header.Get("User-Agent") != "vbilling/test" {
		t.Errorf("proxy request %s (User-Agent %q)", req.URL.Path, req.Header.Get("User-Agent"))
	}
}

func TestBadAccessKeyFailsLoudly(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	r, err := New(Options{Client: ClientOptions{URL: f.srv.URL, AccessKey: "wrong", Insecure: true}, Health: h})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Bind(context.Background(), []discovery.TenantCluster{workload("vc-train-1", "train-1", old, nil)}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("bad key: %v", err)
	}
	if status(h, "platform") != health.Failing {
		t.Errorf("health: %v", h.List())
	}
	if _, err := NewClient(ClientOptions{URL: "platform.example.com", AccessKey: "k"}); err == nil {
		t.Errorf("a URL without scheme must be rejected")
	}
}

func TestDiscoveryUsesBinder(t *testing.T) {
	f := newFakePlatform(t, "platform-4.12.7")
	clk := newClock(time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC))
	r, _ := newResolver(t, f, "cp-1", "", clk)
	kube := fakeKube(t, "vc-train-1", "train-1")
	got, err := discovery.NewDiscoverer(kube, nil, nil, "cp-1").WithBinder(r).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TenantID(discovery.TenantPerPlatform) != "tenant-acme" || got[0].Instance != "train-1" || got[0].DisplayName != "Training 1" {
		t.Fatalf("discovered: %+v", got)
	}
}

func fakeKube(t *testing.T, ns, name string) kubernetes.Interface {
	t.Helper()
	return fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{discovery.AnnotationPrefix + "tenant": "globex"}}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-sts-" + name), Labels: map[string]string{"app": "vcluster"},
			CreationTimestamp: metav1.NewTime(old)}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}},
	)
}

// The access key is sent with every management API request. An answer
// that redirects to another host comes back as an error; the key never
// goes there.
func TestManagementAPINeverFollowsARedirect(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer other.Close()
	plat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer plat.Close()
	c, err := NewClient(ClientOptions{URL: plat.URL, AccessKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.list(context.Background(), projectsGVR); err == nil {
		t.Fatal("a redirected list must fail")
	}
	if _, err := c.servesTenants(); err == nil {
		t.Fatal("a redirected discovery must fail")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) > 0 {
		t.Fatalf("the access key followed a redirect to another host: %q", leaked)
	}
}
