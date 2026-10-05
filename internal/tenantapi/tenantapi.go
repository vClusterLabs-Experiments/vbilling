// Package tenantapi connects vBilling to tenant clusters' own API servers,
// for tenant clusters whose workloads never reach the control plane cluster
// (vCluster Private Nodes, Auto Nodes, Standalone).
//
// Credentials, in order:
//   - external tenant clusters (Standalone, or any cluster elsewhere) use a
//     kubeconfig file listed in TENANT_CLUSTERS_FILE;
//   - tenant clusters on this control plane cluster use the Secret named
//     TENANT_KUBECONFIG_SECRET (default vbilling-kubeconfig) in their
//     namespace. vCluster writes it through exportKubeConfig.additionalSecrets
//     with a read-only service account, so vBilling never holds admin access;
//   - with TENANT_ADMIN_FALLBACK=true, the tenant cluster's admin kubeconfig
//     (Secret vc-<name>) is used when no dedicated Secret exists.
//
// Kubeconfigs that point at localhost (the vCluster default) are rewritten to
// the tenant cluster's Service, https://<name>.<namespace>.svc:443, verified
// as <name>.<namespace> (a name vCluster's serving certificate includes).
package tenantapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/yaml"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
)

// DefaultSecretName is the Secret vBilling reads in a tenant cluster's namespace.
const DefaultSecretName = "vbilling-kubeconfig"

// External is a tenant cluster reached through a kubeconfig file.
type External struct {
	Name        string            `json:"name"`
	Kubeconfig  string            `json:"kubeconfig"`
	Tenant      string            `json:"tenant,omitempty"`
	DisplayName string            `json:"displayName,omitempty"`
	Project     string            `json:"project,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"` // other vbilling.vcluster.com/ settings: email, currency, plan, tenant-class
}

// LoadExternal reads TENANT_CLUSTERS_FILE (a YAML list of External).
func LoadExternal(path string) ([]External, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []External
	if err := yaml.UnmarshalStrict(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, e := range out {
		if e.Name == "" || e.Kubeconfig == "" {
			return nil, fmt.Errorf("%s: every tenant cluster needs name and kubeconfig", path)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("%s: duplicate tenant cluster %q", path, e.Name)
		}
		seen[e.Name] = true
	}
	return out, nil
}

// Cluster is the discovered tenant cluster for an external entry.
func (e External) Cluster() discovery.TenantCluster {
	ann := map[string]string{}
	for k, v := range e.Metadata {
		ann[discovery.AnnotationPrefix+k] = v
	}
	if e.Tenant != "" {
		ann[discovery.AnnotationPrefix+"tenant"] = e.Tenant
	}
	return discovery.TenantCluster{Name: e.Name, Instance: e.Name, External: true, Ready: true, Source: "external",
		Project: e.Project, DisplayName: e.DisplayName, Annotations: ann}
}

// Options configure the Manager.
type Options struct {
	SecretName    string
	AdminFallback bool
	External      []External
	WatchPods     bool          // watch pod deletions too (usage-mode billing)
	Retention     time.Duration // how long deleted nodes and pods are kept
	Timeout       time.Duration
	Now           func() time.Time
}

// Manager implements metrics.TenantAPI with cached clients per tenant cluster.
type Manager struct {
	host    kubernetes.Interface
	ctx     context.Context
	opts    Options
	ext     map[string]External
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	conn    *metrics.TenantConn
	version string
	cancel  context.CancelFunc
}

// New returns a Manager. ctx bounds the deletion watches it starts.
func New(ctx context.Context, host kubernetes.Interface, opts Options) *Manager {
	if opts.SecretName == "" {
		opts.SecretName = DefaultSecretName
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &Manager{host: host, ctx: ctx, opts: opts, ext: map[string]External{}, entries: map[string]*entry{}}
	for _, e := range opts.External {
		m.ext[e.Name] = e
	}
	return m
}

// Clusters returns the external tenant clusters, for discovery.
func (m *Manager) Clusters() []discovery.TenantCluster {
	out := make([]discovery.TenantCluster, 0, len(m.opts.External))
	for _, e := range m.opts.External {
		out = append(out, e.Cluster())
	}
	return out
}

// Conn returns a connection to the tenant cluster's API, or
// metrics.ErrNoTenantAPI when none is configured for it.
func (m *Manager) Conn(ctx context.Context, cl discovery.TenantCluster) (*metrics.TenantConn, error) {
	key := cl.ExternalID()
	cfg, version, err := m.restConfig(ctx, cl)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		if e.version == version {
			return e.conn, nil
		}
		e.cancel() // credentials rotated
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mc, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	g := metrics.NewGraveyard(m.opts.Retention, m.opts.Now)
	wctx, cancel := context.WithCancel(m.ctx)
	go g.Watch(wctx, kube, "", m.opts.WatchPods, true)
	conn := &metrics.TenantConn{Kube: kube, Metrics: mc, Dynamic: dyn, Graveyard: g, External: cl.External}
	m.entries[key] = &entry{conn: conn, version: version, cancel: cancel}
	log.Printf("[tenantapi] %s: metering through the tenant cluster's API at %s", key, cfg.Host)
	return conn, nil
}

// Retain stops watches of tenant clusters that are gone.
func (m *Manager) Retain(clusters []discovery.TenantCluster) {
	keep := map[string]bool{}
	for i := range clusters {
		keep[clusters[i].ExternalID()] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.entries {
		if !keep[k] {
			e.cancel()
			delete(m.entries, k)
		}
	}
}

func (m *Manager) restConfig(ctx context.Context, cl discovery.TenantCluster) (*rest.Config, string, error) {
	if cl.External {
		e, ok := m.ext[cl.Name]
		if !ok {
			return nil, "", metrics.ErrNoTenantAPI
		}
		data, err := os.ReadFile(e.Kubeconfig)
		if err != nil {
			return nil, "", fmt.Errorf("read kubeconfig: %w", err)
		}
		cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
		if err != nil {
			return nil, "", fmt.Errorf("parse kubeconfig %s: %w", e.Kubeconfig, err)
		}
		cfg.Timeout = m.opts.Timeout
		sum := sha256.Sum256(data)
		return cfg, hex.EncodeToString(sum[:8]), nil
	}
	names := []string{m.opts.SecretName}
	if m.opts.AdminFallback {
		names = append(names, "vc-"+cl.Name)
	}
	for _, name := range names {
		sec, err := m.host.CoreV1().Secrets(cl.Namespace).Get(ctx, name, metav1.GetOptions{})
		if kerrors.IsNotFound(err) || kerrors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("read secret %s/%s: %w", cl.Namespace, name, err)
		}
		data := firstKey(sec.Data, "config", "kubeconfig", "value")
		if data == nil {
			return nil, "", fmt.Errorf("secret %s/%s has no kubeconfig (keys config, kubeconfig or value)", cl.Namespace, name)
		}
		cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
		if err != nil {
			return nil, "", fmt.Errorf("parse kubeconfig in secret %s/%s: %w", cl.Namespace, name, err)
		}
		if host, serverName, ok := inCluster(cfg.Host, cl.Name, cl.Namespace); ok {
			cfg.Host, cfg.TLSClientConfig.ServerName = host, serverName
		}
		cfg.Timeout = m.opts.Timeout
		return cfg, name + "@" + sec.ResourceVersion, nil
	}
	return nil, "", metrics.ErrNoTenantAPI
}

// inCluster points kubeconfigs written for local port-forwarding (the
// vCluster default, https://localhost:8443) at the tenant cluster's Service.
// The serving certificate covers <name>.<namespace> but not the .svc form,
// so that is the name TLS verifies.
func inCluster(host, name, namespace string) (newHost, serverName string, ok bool) {
	u, err := url.Parse(host)
	if err != nil {
		return "", "", false
	}
	switch h := u.Hostname(); {
	case h == "localhost", net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback():
		return fmt.Sprintf("https://%s.%s.svc:443", name, namespace), name + "." + namespace, true
	}
	return "", "", false
}

func firstKey(data map[string][]byte, keys ...string) []byte {
	for _, k := range keys {
		if v, ok := data[k]; ok && len(v) > 0 {
			return v
		}
	}
	return nil
}
