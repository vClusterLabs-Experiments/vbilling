package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/vclusterlabs-experiments/vbilling/internal/api"
	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/controller"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/tenantapi"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"

	// Register the remaining built-in adapters.
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/lago"
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/noop"
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/webhook"
)

var version = "0.2.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("vBilling %s: usage metering for tenant clusters", version)

	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	if cfg.SpotDiscountPercent > 0 {
		log.Printf("WARNING: SPOT_DISCOUNT_PERCENT is deprecated and ignored. Quantities are no longer discounted; " +
			"price spot and preemptible capacity in your billing backend using the capacity_type dimension.")
	}
	customs, err := usage.ParseCustomMetrics(cfg.CustomMetrics)
	if err != nil {
		log.Fatalf("CUSTOM_METRICS: %v", err)
	}
	for _, d := range customs {
		if err := usage.RegisterCustom(d); err != nil {
			log.Fatalf("CUSTOM_METRICS: %v", err)
		}
	}
	promMetrics, err := metrics.LoadPromMetrics(cfg.PrometheusMetrics)
	if err != nil {
		log.Fatalf("PROMETHEUS_METRICS_FILE: %v", err)
	}
	log.Printf("Adapters: %v | Region: %s | Window: %s | CPU/memory basis: %s | Data: %s",
		cfg.Adapters, cfg.Region, cfg.CollectionInterval, cfg.CPUMemoryBasis, cfg.DataDir)

	sp, err := spool.Open(filepath.Join(cfg.DataDir, "ledger"), spool.Options{Retention: cfg.Retention})
	if err != nil {
		log.Fatalf("open ledger: %v", err)
	}
	dests, err := destinations.NewAll(cfg.Adapters, cfg)
	if err != nil {
		log.Fatalf("initialize adapters: %v", err)
	}

	kubeConfig, err := getKubeConfig()
	if err != nil {
		log.Fatalf("kubernetes config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	metricsClient, err := metricsclient.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("metrics client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	tel := telemetry.Default
	tenants := pipeline.NewTenantRegistry()
	disp := pipeline.New(sp, dests, pipeline.Options{
		Tenants:   tenants,
		Bootstrap: func(ctx context.Context, d destinations.Destination) error { return d.Bootstrap(ctx, usage.Catalog()) },
	}, tel)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	disc := discovery.NewDiscoverer(kube, dynamicClient, cfg.WatchNamespaces, cfg.PlatformCluster)
	coll := metrics.NewCollector(kube, metricsClient, metrics.Options{
		Region:             cfg.Region,
		RegionFromNode:     cfg.RegionFromNode,
		Basis:              cfg.CPUMemoryBasis,
		MeterControlPlane:  cfg.MeterControlPlane,
		MeterByNamespace:   cfg.MeterByNamespace,
		GPUResources:       cfg.GPUResources,
		SKULabel:           cfg.SKULabel,
		CapacityTypeLabel:  cfg.CapacityTypeLabel,
		UnhealthyTaints:    cfg.UnhealthyTaints,
		DedicatedNodeLabel: cfg.DedicatedNodeLabel,
		PrometheusURL:      cfg.PrometheusURL,
		PromHeaders:        cfg.PrometheusHeaders,
		PromTokenFile:      cfg.PromTokenFile,
		EgressQuery:        cfg.EgressQuery,
		GPUUtilQuery:       cfg.GPUUtilQuery,
		GPUCountQuery:      cfg.GPUCountQuery,
		PrometheusPreset:   cfg.PrometheusPreset,
		PromMetrics:        promMetrics,

		PrivateNodeBilling:      cfg.PrivateNodeBilling,
		TenantExcludeNamespaces: cfg.TenantExcludeNamespaces,
		DRAGPUDrivers:           cfg.DRAGPUDrivers,
	})
	coll.UseDynamic(dynamicClient)
	// Deleted pods and nodes stay billable until their windows are committed.
	retention := cfg.MaxBackfill + 2*cfg.CollectionInterval
	if cfg.WatchDeletions {
		g := metrics.NewGraveyard(retention, time.Now)
		go g.Watch(ctx, kube, metrics.LabelManagedBy, true, true)
		coll.UseGraveyard(g)
	}
	if cfg.TenantAPI {
		ext, err := tenantapi.LoadExternal(cfg.TenantClustersFile)
		if err != nil {
			log.Fatalf("TENANT_CLUSTERS_FILE: %v", err)
		}
		mgr := tenantapi.New(ctx, kube, tenantapi.Options{
			SecretName:    cfg.TenantKubeconfigSecret,
			AdminFallback: cfg.TenantAdminFallback,
			External:      ext,
			WatchPods:     cfg.PrivateNodeBilling == "usage",
			Retention:     retention,
		})
		coll.UseTenantAPI(mgr)
		disc.WithExternal(mgr.Clusters())
		log.Printf("Tenant APIs: Secret %q per tenant cluster (admin fallback %t), %d external tenant cluster(s), private nodes billed by %s",
			cfg.TenantKubeconfigSecret, cfg.TenantAdminFallback, len(ext), cfg.PrivateNodeBilling)
	}
	ctrl := controller.New(cfg, disc, coll, sp, tenants, dests, tel)
	enf := newEnforcer(cfg, kube, ctrl, dests, tel)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(cfg, sp, disp, tenants, ctrl, enf, tel, version).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.APIToken == "" {
		log.Printf("WARNING: API_TOKEN is not set; usage and admin endpoints on %s are unauthenticated (keep the Service internal)", cfg.ListenAddr)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		log.Printf("API listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("api server: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		disp.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		compactLoop(ctx, sp, cfg.Adapters)
	}()

	if err := ctrl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("controller: %v", err)
	}
	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	srv.Shutdown(shutdownCtx)
	wg.Wait()
	if err := sp.Close(); err != nil {
		log.Printf("close ledger: %v", err)
	}
	log.Println("vBilling stopped")
}

// newEnforcer sets up billing states. They are always available: Stripe and
// Metronome webhooks set them once their secrets are configured, and
// PUT /api/v1/billing-states/{tenant} sets them from anywhere else, such as
// Lago or a custom billing platform.
func newEnforcer(cfg *config.Config, kube kubernetes.Interface, ctrl *controller.Controller, dests []destinations.Destination, tel *telemetry.Registry) *enforcement.Enforcer {
	rules, err := enforcement.ParseRules(cfg.EnforcementRules)
	if err != nil {
		log.Fatalf("ENFORCEMENT_RULES: %v", err)
	}
	resolvers := map[string]destinations.CustomerResolver{}
	for _, d := range dests {
		switch a := d.(type) {
		case *stripe.Adapter:
			resolvers["stripe"] = a
		case *metronome.Adapter:
			resolvers["metronome"] = a
			if link := a.StripeLink(); link != nil {
				if _, ok := resolvers["stripe"]; !ok {
					resolvers["stripe"] = link // Stripe dunning for Metronome-rated tenants
				}
			}
		}
	}
	state, err := kv.Open(filepath.Join(cfg.DataDir, "billing-state.json"))
	if err != nil {
		log.Fatalf("open billing state: %v", err)
	}
	log.Printf("Billing states: mode=%s, Stripe webhooks %s, Metronome webhooks %s", cfg.EnforcementMode,
		onOff(cfg.StripeWebhookSecret != ""), onOff(cfg.MetronomeWebhookSecret != ""))
	return enforcement.New(kube, enforcement.Options{
		Mode:                   cfg.EnforcementMode,
		StripeWebhookSecret:    cfg.StripeWebhookSecret,
		MetronomeWebhookSecret: cfg.MetronomeWebhookSecret,
		Rules:                  rules,
		Clusters:               ctrl.ClustersOf,
		Resolvers:              resolvers,
		State:                  state,
		Telemetry:              tel,
	})
}

// compactLoop applies ledger retention hourly.
func compactLoop(ctx context.Context, sp *spool.Spool, consumers []string) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := sp.Compact(consumers); err != nil {
				log.Printf("ledger compaction: %v", err)
			} else if n > 0 {
				log.Printf("ledger compaction removed %d segment(s) past retention", n)
			}
		}
	}
}

func getKubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		log.Println("Using in-cluster Kubernetes config")
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	log.Printf("Using kubeconfig (%s)", os.Getenv("KUBECONFIG"))
	return cfg, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
