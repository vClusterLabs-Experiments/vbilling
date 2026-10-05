// Package config loads vBilling settings from environment variables.
// Every v0.1 variable keeps working; ADAPTER is still accepted as a single
// entry of ADAPTERS.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Adapters lists the destinations usage fans out to, e.g.
	// ["metronome", "webhook"]. Built-in: lago, stripe, metronome, webhook, noop.
	Adapters []string

	// Where this vBilling instance meters. Region is stamped on every event;
	// a node's topology.kubernetes.io/region label wins when present.
	Region      string
	Zone        string
	ClusterName string // name of the control plane cluster (event source, webhook source)
	TenantClass string // default tenant class, e.g. public, enterprise, government, dev

	// Durable ledger.
	DataDir   string
	Retention time.Duration

	// Collection. CollectionInterval is the metering window size.
	CollectionInterval time.Duration
	ReconcileInterval  time.Duration
	MaxBackfill        time.Duration
	CPUMemoryBasis     string // usage | requests | max
	MeterControlPlane  bool
	MeterByNamespace   bool
	GPUResources       []string
	SKULabel           string
	CapacityTypeLabel  string
	UnhealthyTaints    []string
	PrometheusURL      string
	PrometheusHeaders  map[string]string // extra request headers, e.g. X-Scope-OrgID or Authorization
	PromTokenFile      string            // bearer token file, re-read on every query
	EgressQuery        string
	GPUUtilQuery       string // replaces the DCGM utilization query; "none" disables it
	GPUCountQuery      string
	PrometheusPreset   string // "vcluster-platform": vCluster Platform fleet observability labels
	PrometheusMetrics  string // PROMETHEUS_METRICS_FILE: operator-defined metrics from PromQL
	DRAGPUDrivers      []string
	WatchDeletions     bool // bill pods and nodes deleted between windows

	// Tenant clusters with their own nodes (Private Nodes, Auto Nodes, Standalone).
	TenantAPI               bool
	TenantKubeconfigSecret  string
	TenantAdminFallback     bool
	TenantClustersFile      string
	PrivateNodeBilling      string // node | usage
	TenantExcludeNamespaces []string
	WatchNamespaces         []string
	DedicatedNodeLabel      string // extra node selector for dedicated nodes, "key=%s" (%s = cluster name)
	RegionFromNode          bool   // use topology.kubernetes.io/region from nodes instead of REGION
	TenantSource            string // cluster | project
	PlatformCluster         string
	OffboardGrace           time.Duration
	CustomMetrics           string

	// Billing defaults.
	DefaultPlanCode string
	BillingCurrency string

	// HTTP API.
	ListenAddr  string
	APIToken    string
	IngestToken string

	// Enforcement of billing state on tenant clusters: observe | annotate | enforce.
	EnforcementMode  string
	EnforcementRules string // "event.type=state,..." overrides

	// Lago.
	LagoAPIURL string
	LagoAPIKey string

	// Stripe (Billing Meters).
	StripeAPIKey         string
	StripeAPIBase        string
	StripeAPIVersion     string
	StripeWebhookSecret  string
	StripeSplitMetersBy  []string
	StripeAutoSubscribe  bool
	StripeMaxRPS         float64
	StripeCancelOnRemove bool

	// Metronome.
	MetronomeAPIToken      string
	MetronomeAPIBase       string
	MetronomeWebhookSecret string
	MetronomeRateCard      string // rate card ID or alias for automatic contracts
	MetronomeStripeLink    bool   // create the Stripe customer and link it for invoicing
	MetronomeStripeCollect string // charge_automatically | send_invoice

	// Webhook (CloudEvents).
	WebhookURL     string
	WebhookSecret  string
	WebhookHeaders map[string]string

	// Deprecated: discounts belong in the rating engine, keyed on the
	// capacity_type dimension. Kept so v0.1 Helm values still parse.
	SpotDiscountPercent float64
}

func Load() *Config {
	c := &Config{
		Adapters:    envList("ADAPTERS", nil),
		Region:      envOr("REGION", "default"),
		Zone:        os.Getenv("ZONE"),
		ClusterName: envOr("CLUSTER_NAME", "control-plane"),
		TenantClass: os.Getenv("DEFAULT_TENANT_CLASS"),

		DataDir:   envOr("DATA_DIR", "/var/lib/vbilling"),
		Retention: envDuration("RETENTION", 7*24*time.Hour),

		CollectionInterval: envDuration("COLLECTION_INTERVAL", 60*time.Second),
		ReconcileInterval:  envDuration("RECONCILE_INTERVAL", 30*time.Second),
		MaxBackfill:        envDuration("MAX_BACKFILL", 6*time.Hour),
		CPUMemoryBasis:     envOr("CPU_MEMORY_BASIS", "usage"),
		MeterControlPlane:  envBool("METER_CONTROL_PLANE", false),
		MeterByNamespace:   envBool("METER_BY_NAMESPACE", false),
		GPUResources:       envList("GPU_RESOURCES", []string{"nvidia.com/gpu", "amd.com/gpu"}),
		SKULabel:           os.Getenv("SKU_LABEL"),
		CapacityTypeLabel:  envOr("CAPACITY_TYPE_LABEL", "vbilling.vcluster.com/capacity-type"),
		UnhealthyTaints:    envList("UNHEALTHY_NODE_TAINTS", []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"}),
		PrometheusURL:      os.Getenv("PROMETHEUS_URL"),
		PrometheusHeaders:  envMap("PROMETHEUS_HEADERS"),
		PromTokenFile:      os.Getenv("PROMETHEUS_BEARER_TOKEN_FILE"),
		EgressQuery:        os.Getenv("EGRESS_QUERY"),
		GPUUtilQuery:       os.Getenv("GPU_UTIL_QUERY"),
		GPUCountQuery:      os.Getenv("GPU_COUNT_QUERY"),
		PrometheusPreset:   os.Getenv("PROMETHEUS_PRESET"),
		PrometheusMetrics:  os.Getenv("PROMETHEUS_METRICS_FILE"),
		DRAGPUDrivers:      envList("DRA_GPU_DRIVERS", []string{"gpu.nvidia.com", "gpu.amd.com"}),
		WatchDeletions:     envBool("WATCH_DELETIONS", true),

		TenantAPI:               envBool("TENANT_API", true),
		TenantKubeconfigSecret:  envOr("TENANT_KUBECONFIG_SECRET", "vbilling-kubeconfig"),
		TenantAdminFallback:     envBool("TENANT_ADMIN_FALLBACK", false),
		TenantClustersFile:      os.Getenv("TENANT_CLUSTERS_FILE"),
		PrivateNodeBilling:      envOr("PRIVATE_NODE_BILLING", "node"),
		TenantExcludeNamespaces: envList("TENANT_EXCLUDE_NAMESPACES", []string{"kube-system"}),
		WatchNamespaces:         envList("WATCH_NAMESPACES", nil),
		DedicatedNodeLabel:      os.Getenv("VCLUSTER_NODE_LABEL"),
		RegionFromNode:          envBool("REGION_FROM_NODE", false),
		TenantSource:            envOr("TENANT_SOURCE", "cluster"),
		PlatformCluster:         os.Getenv("PLATFORM_CLUSTER"),
		OffboardGrace:           envDuration("OFFBOARD_GRACE", time.Hour),
		CustomMetrics:           os.Getenv("CUSTOM_METRICS"),

		DefaultPlanCode: envOr("DEFAULT_PLAN_CODE", "vcluster-standard"),
		BillingCurrency: envOr("BILLING_CURRENCY", "USD"),

		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		APIToken:    secretEnv("API_TOKEN"),
		IngestToken: secretEnv("INGEST_TOKEN"),

		EnforcementMode:  envOr("ENFORCEMENT_MODE", "observe"),
		EnforcementRules: os.Getenv("ENFORCEMENT_RULES"),

		LagoAPIURL: envOr("LAGO_API_URL", "http://localhost:3000"),
		LagoAPIKey: secretEnv("LAGO_API_KEY"),

		StripeAPIKey:         secretEnv("STRIPE_API_KEY"),
		StripeAPIBase:        envOr("STRIPE_API_BASE", "https://api.stripe.com"),
		StripeAPIVersion:     envOr("STRIPE_API_VERSION", "2026-09-30.endive"),
		StripeWebhookSecret:  secretEnv("STRIPE_WEBHOOK_SECRET"),
		StripeSplitMetersBy:  envList("STRIPE_SPLIT_METERS_BY", []string{"sku"}),
		StripeAutoSubscribe:  envBool("STRIPE_AUTO_SUBSCRIBE", false),
		StripeMaxRPS:         envFloat("STRIPE_MAX_RPS", 0),
		StripeCancelOnRemove: envBool("STRIPE_CANCEL_ON_REMOVE", false),

		MetronomeAPIToken:      secretEnv("METRONOME_API_TOKEN"),
		MetronomeAPIBase:       envOr("METRONOME_API_BASE", "https://api.metronome.com"),
		MetronomeWebhookSecret: secretEnv("METRONOME_WEBHOOK_SECRET"),
		MetronomeRateCard:      os.Getenv("METRONOME_RATE_CARD"),
		MetronomeStripeLink:    envBool("METRONOME_STRIPE_LINK", false),
		MetronomeStripeCollect: envOr("METRONOME_STRIPE_COLLECTION_METHOD", "charge_automatically"),

		WebhookURL:     os.Getenv("WEBHOOK_URL"),
		WebhookSecret:  secretEnv("WEBHOOK_SECRET"),
		WebhookHeaders: envMap("WEBHOOK_HEADERS"),

		SpotDiscountPercent: envFloat("SPOT_DISCOUNT_PERCENT", 0),
	}
	if len(c.Adapters) == 0 {
		c.Adapters = []string{envOr("ADAPTER", "lago")}
	}
	if c.IngestToken == "" {
		c.IngestToken = c.APIToken
	}
	return c
}

// Validate rejects settings that would silently produce wrong bills.
func (c *Config) Validate() error {
	if c.CollectionInterval < 10*time.Second || c.CollectionInterval > time.Hour {
		return fmt.Errorf("COLLECTION_INTERVAL %s must be between 10s and 1h", c.CollectionInterval)
	}
	if time.Hour%c.CollectionInterval != 0 {
		return fmt.Errorf("COLLECTION_INTERVAL %s must divide an hour evenly so windows align with billing hours", c.CollectionInterval)
	}
	switch c.CPUMemoryBasis {
	case "usage", "requests", "max":
	default:
		return fmt.Errorf("CPU_MEMORY_BASIS %q must be usage, requests or max", c.CPUMemoryBasis)
	}
	switch c.TenantSource {
	case "cluster", "project":
	default:
		return fmt.Errorf("TENANT_SOURCE %q must be cluster or project", c.TenantSource)
	}
	switch c.EnforcementMode {
	case "observe", "annotate", "enforce":
	default:
		return fmt.Errorf("ENFORCEMENT_MODE %q must be observe, annotate or enforce", c.EnforcementMode)
	}
	switch c.PrivateNodeBilling {
	case "node", "usage":
	default:
		return fmt.Errorf("PRIVATE_NODE_BILLING %q must be node or usage", c.PrivateNodeBilling)
	}
	switch c.PrometheusPreset {
	case "", "default", "vcluster-platform":
	default:
		return fmt.Errorf("PROMETHEUS_PRESET %q must be empty, default or vcluster-platform", c.PrometheusPreset)
	}
	return nil
}

// secretEnv reads a key or token. Secrets mounted from files usually end in a
// newline, which would make every request's Authorization header invalid.
func secretEnv(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// envList parses a comma-separated list. An explicitly empty value ("none")
// yields an empty, non-nil list so defaults can be switched off.
func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	out := []string{}
	if strings.TrimSpace(v) == "none" {
		return out
	}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envMap parses "k1=v1,k2=v2".
func envMap(key string) map[string]string {
	out := map[string]string{}
	for _, kv := range envList(key, nil) {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
