package tenantapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
)

func kubeconfig(server string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: t
  cluster: {server: ` + server + `, insecure-skip-tls-verify: true}
users:
- name: reader
  user: {token: read-only-token}
contexts:
- name: t
  context: {cluster: t, user: reader}
current-context: t
`)
}

func secret(ns, name, server string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, ResourceVersion: "1"},
		Data: map[string][]byte{"config": kubeconfig(server)}}
}

var team = discovery.TenantCluster{Name: "team-p", Namespace: "team-p"}

func TestLeastPrivilegeSecretAndInClusterAddress(t *testing.T) {
	host := fake.NewSimpleClientset(secret("team-p", DefaultSecretName, "https://localhost:8443"))
	m := New(context.Background(), host, Options{})
	cfg, version, err := m.restConfig(context.Background(), team)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://team-p.team-p.svc:443" || cfg.TLSClientConfig.ServerName != "team-p.team-p" {
		t.Errorf("host = %s (TLS name %q), want the tenant cluster's Service verified as team-p.team-p", cfg.Host, cfg.TLSClientConfig.ServerName)
	}
	if cfg.BearerToken != "read-only-token" || !strings.HasPrefix(version, DefaultSecretName+"@") {
		t.Errorf("credentials from %s, token %q", version, cfg.BearerToken)
	}
	for _, in := range []string{"https://127.0.0.1:8443", "https://[::1]:8443"} {
		if got, name, ok := inCluster(in, "a", "b"); !ok || got != "https://a.b.svc:443" || name != "a.b" {
			t.Errorf("inCluster(%s) = %s, %s, %v", in, got, name, ok)
		}
	}
	if _, _, ok := inCluster("https://team-p.example.com:6443", "a", "b"); ok {
		t.Error("a reachable server must be kept")
	}
}

func TestAdminKubeconfigOnlyWhenAllowed(t *testing.T) {
	host := fake.NewSimpleClientset(secret("team-p", "vc-team-p", "https://localhost:8443"))
	if _, err := New(context.Background(), host, Options{}).Conn(context.Background(), team); !errors.Is(err, metrics.ErrNoTenantAPI) {
		t.Fatalf("without TENANT_ADMIN_FALLBACK the admin kubeconfig must not be used, err = %v", err)
	}
	m := New(context.Background(), host, Options{AdminFallback: true})
	if _, version, err := m.restConfig(context.Background(), team); err != nil || !strings.HasPrefix(version, "vc-team-p@") {
		t.Fatalf("admin fallback: %s, %v", version, err)
	}
}

func TestConnCachedUntilCredentialsRotate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := fake.NewSimpleClientset(secret("team-p", DefaultSecretName, "https://localhost:8443"))
	m := New(ctx, host, Options{Retention: time.Hour})
	a, err := m.Conn(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := m.Conn(ctx, team); b != a {
		t.Error("connection not reused")
	}
	rotated := secret("team-p", DefaultSecretName, "https://localhost:8443")
	rotated.ResourceVersion = "2"
	if _, err := host.CoreV1().Secrets("team-p").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if c, _ := m.Conn(ctx, team); c == a {
		t.Error("rotated credentials must build a new connection")
	}
	m.Retain(nil)
	if len(m.entries) != 0 {
		t.Error("connections of removed tenant clusters must be dropped")
	}
}

func TestExternalTenantClusters(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "gpu-standalone.kubeconfig")
	if err := os.WriteFile(kc, kubeconfig("https://10.0.0.5:6443"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "clusters.yaml")
	body := "- name: gpu-standalone\n  kubeconfig: " + kc + "\n  tenant: acme\n  displayName: Acme AI\n  metadata: {currency: aud}\n"
	if err := os.WriteFile(list, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ext, err := LoadExternal(list)
	if err != nil {
		t.Fatal(err)
	}
	cl := ext[0].Cluster()
	if cl.ExternalID() != "external-gpu-standalone" || cl.TenantID(discovery.TenantPerCluster) != "acme" || cl.Meta("currency") != "aud" || !cl.External {
		t.Fatalf("external cluster = %+v", cl)
	}
	m := New(context.Background(), fake.NewSimpleClientset(), Options{External: ext})
	cfg, _, err := m.restConfig(context.Background(), cl)
	if err != nil || cfg.Host != "https://10.0.0.5:6443" {
		t.Fatalf("external kubeconfig: %v, %v", cfg, err)
	}
	if _, err := LoadExternal(writeFile(t, dir, "bad.yaml", "- name: x\n")); err == nil {
		t.Error("an entry without kubeconfig must be rejected")
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
