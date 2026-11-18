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
	DataDir    string
	Retention  time.Duration
	ArchiveDir string // ARCHIVE_DIR: gzipped copies of compacted segments, kept for disputes

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
	// PromSourceLabel names the source cluster on series from clusters with
	// their own nodes (an external label of their Prometheus agent).
	PromSourceLabel string
	// PromLookback is how far before a window a counter's starting sample may be.
	PromLookback time.Duration

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
	TenantSource            string // cluster | project | platform
	PlatformCluster         string
	OffboardGrace           time.Duration
	CustomMetrics           string
	CustomMetricsFile       string // YAML list form of CUSTOM_METRICS (Helm metering.customMetrics)
	// PlanByClass picks a plan (Lago plan code, Stripe plan tag, Metronome
	// rate card) for tenants without a vbilling.vcluster.com/plan setting.
	PlanByClass map[string]string

	// TENANT_SOURCE=platform: tenant identity from vCluster Platform objects.
	PlatformURL         string
	PlatformAccessKey   string
	PlatformCAFile      string
	PlatformInsecure    bool
	PlatformRefresh     time.Duration
	PlatformOrphanGrace time.Duration
	PlatformProxy       bool // reach clusters with their own nodes through the Platform proxy

	// Per-request usage records (POST /api/v1/records).
	// CollectionDelay is how long after a window ends it is metered (at
	// least 5s). 75s lets Kubernetes mark a node that died without warning
	// before its last window is committed.
	CollectionDelay      time.Duration
	RecordsSettle        time.Duration
	RecordsRetention     time.Duration
	RecordsMappingsFile  string
	RecordsAllowUnkeyed  bool
	RecordsRejectUnknown bool
	// RecordsArchive also archives per-request records (to ARCHIVE_DIR/records)
	// before retention removes them: request-level evidence for disputes.
	RecordsArchive bool
	// RecordsLiteLLM enables the built-in litellm payload format.
	RecordsLiteLLM bool
	IngestTokens   map[string]string // token -> tenant the token is bound to
	// RecordsPullFile lists gateway request logs to read back as records
	// (YAML, see records.PullSpec), for any gateway with a logs API.
	RecordsPullFile string
	// RecordsPullLiteLLM* read LiteLLM's spend logs back as records, to fill
	// what the generic API logger's push lost. Setting the URL enables it,
	// and the litellm format with it.
	RecordsPullLiteLLMURL      string
	RecordsPullLiteLLMKey      string
	RecordsPullLiteLLMClient   string // must equal the push's client (X-VBilling-Client)
	RecordsPullLiteLLMTenant   string // like a tenant-bound ingest token
	RecordsPullLiteLLMInterval time.Duration
	RecordsPullLiteLLMLookback time.Duration
	// CrossChecks compares two sources of the same usage per tenant and hour:
	// "primary=secondary[:tolerance%],...".
	CrossChecks string

	// Billing defaults.
	DefaultPlanCode string
	BillingCurrency string

	// Price coverage (internal/pricing): warn reports usage a billing
	// platform has no price for; hold also keeps that usage from it until
	// the price exists; off disables the check.
	PriceCheck         string
	PriceCheckInterval time.Duration
	PriceCheckLookback time.Duration
	// PriceCheckMaxHold releases held usage without a price once its window
	// started this long ago, before billing platforms refuse it as too old.
	PriceCheckMaxHold time.Duration

	// HTTP API.
	ListenAddr  string
	APIToken    string
	IngestToken string

	// Enforcement of billing state on tenant clusters: observe | annotate | enforce.
	EnforcementMode  string
	EnforcementRules string // "event.type=state,..." overrides
	// EnforcementInCluster also installs an admission policy inside each
	// suspended tenant's clusters through their own API (enforce mode). It
	// is the only enforcement that reaches clusters with their own nodes,
	// and needs write access to admission policies inside them.
	EnforcementInCluster bool
	// EnforcementLiteLLM blocks a suspended or paused tenant's LiteLLM teams
	// (its tenant ID and aliases), using RECORDS_PULL_LITELLM_URL and _KEY.
	EnforcementLiteLLM bool
	// PauseKeepBilling is the capacity that keeps billing while a cluster
	// is paused: storage, load_balancers, nodes, control_plane, workloads, or none.
	PauseKeepBilling string
	// PauseDrainTimeout bounds the wait for workloads on a cluster's own
	// nodes to stop before its control plane is stopped.
	PauseDrainTimeout time.Duration

	// Lago.
	LagoAPIURL       string
	LagoAPIKey       string
	LagoPriceCard    string   // YAML amounts for the charges and SKU charge filters vBilling creates
	LagoManagedPlans []string // plans besides DefaultPlanCode whose charges and SKU filters vBilling manages

	// Stripe (Billing Meters).
	StripeAPIKey         string
	StripeAPIBase        string
	StripeAPIVersion     string
	StripeWebhookSecret  string
	StripeSplitMetersBy  []string
	StripeAutoSubscribe  bool
	StripeMaxRPS         float64
	StripeCancelOnRemove bool
	// StripePriceCard is a YAML file of plan prices vBilling creates in
	// Stripe when missing (never changes existing prices).
	StripePriceCard string

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

		DataDir:    envOr("DATA_DIR", "/var/lib/vbilling"),
		Retention:  envDuration("RETENTION", 7*24*time.Hour),
		ArchiveDir: os.Getenv("ARCHIVE_DIR"),

		CollectionInterval: envDuration("COLLECTION_INTERVAL", 60*time.Second),
		ReconcileInterval:  envDuration("RECONCILE_INTERVAL", 30*time.Second),
		MaxBackfill:        envDuration("MAX_BACKFILL", 6*time.Hour),
		CPUMemoryBasis:     envOr("CPU_MEMORY_BASIS", "requests"),
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
		PromSourceLabel:    envOr("PROMETHEUS_SOURCE_LABEL", "vcluster"),
		PromLookback:       envDuration("PROMETHEUS_LOOKBACK", 10*time.Minute),

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
		PlatformURL:             os.Getenv("PLATFORM_URL"),
		PlatformAccessKey:       secretEnv("PLATFORM_ACCESS_KEY"),
		PlatformCAFile:          os.Getenv("PLATFORM_CA_FILE"),
		PlatformInsecure:        envBool("PLATFORM_INSECURE", false),
		PlatformRefresh:         envDuration("PLATFORM_REFRESH", time.Minute),
		PlatformOrphanGrace:     envDuration("PLATFORM_ORPHAN_GRACE", 0),
		PlatformProxy:           envBool("PLATFORM_PROXY", true),
		OffboardGrace:           envDuration("OFFBOARD_GRACE", time.Hour),
		CustomMetrics:           os.Getenv("CUSTOM_METRICS"),
		CustomMetricsFile:       os.Getenv("CUSTOM_METRICS_FILE"),
		PlanByClass:             envMap("PLAN_BY_CLASS"),
		CollectionDelay:         envDuration("COLLECTION_DELAY", 5*time.Second),
		RecordsSettle:           envDuration("RECORDS_SETTLE", 30*time.Second),
		RecordsRetention:        envDuration("RECORDS_RETENTION", 7*24*time.Hour),
		RecordsMappingsFile:     os.Getenv("RECORDS_MAPPINGS_FILE"),
		RecordsAllowUnkeyed:     envBool("RECORDS_ALLOW_UNKEYED", false),
		RecordsRejectUnknown:    os.Getenv("RECORDS_UNKNOWN_TENANTS") == "reject",
		RecordsArchive:          envBool("RECORDS_ARCHIVE", false),
		RecordsLiteLLM:          envBool("RECORDS_LITELLM", false),
		IngestTokens:            secretMap("INGEST_TOKENS"),
		CrossChecks:             os.Getenv("CROSS_CHECKS"),

		RecordsPullLiteLLMURL:      os.Getenv("RECORDS_PULL_LITELLM_URL"),
		RecordsPullFile:            os.Getenv("RECORDS_PULL_FILE"),
		RecordsPullLiteLLMKey:      secretEnv("RECORDS_PULL_LITELLM_KEY"),
		RecordsPullLiteLLMClient:   envOr("RECORDS_PULL_LITELLM_CLIENT", "litellm"),
		RecordsPullLiteLLMTenant:   os.Getenv("RECORDS_PULL_LITELLM_TENANT"),
		RecordsPullLiteLLMInterval: envDuration("RECORDS_PULL_LITELLM_INTERVAL", 5*time.Minute),
		RecordsPullLiteLLMLookback: envDuration("RECORDS_PULL_LITELLM_LOOKBACK", time.Hour),

		DefaultPlanCode: envOr("DEFAULT_PLAN_CODE", "vcluster-standard"),
		BillingCurrency: envOr("BILLING_CURRENCY", "USD"),

		PriceCheck:         envOr("PRICE_CHECK", "warn"),
		PriceCheckInterval: envDuration("PRICE_CHECK_INTERVAL", 10*time.Minute),
		PriceCheckLookback: envDuration("PRICE_CHECK_LOOKBACK", 24*time.Hour),
		PriceCheckMaxHold:  envDays("PRICE_CHECK_MAX_HOLD", 28*24*time.Hour),

		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		APIToken:    secretEnv("API_TOKEN"),
		IngestToken: secretEnv("INGEST_TOKEN"),

		EnforcementMode:      envOr("ENFORCEMENT_MODE", "observe"),
		EnforcementRules:     os.Getenv("ENFORCEMENT_RULES"),
		EnforcementInCluster: envBool("ENFORCEMENT_IN_CLUSTER", false),
		EnforcementLiteLLM:   envBool("ENFORCEMENT_LITELLM", false),
		PauseKeepBilling:     envOr("PAUSE_KEEP_BILLING", "storage,load_balancers"),
		PauseDrainTimeout:    envDuration("PAUSE_DRAIN_TIMEOUT", 2*time.Minute),

		LagoAPIURL:       envOr("LAGO_API_URL", "http://localhost:3000"),
		LagoAPIKey:       secretEnv("LAGO_API_KEY"),
		LagoPriceCard:    os.Getenv("LAGO_PRICE_CARD"),
		LagoManagedPlans: envList("LAGO_MANAGED_PLANS", nil),

		StripeAPIKey:         secretEnv("STRIPE_API_KEY"),
		StripeAPIBase:        envOr("STRIPE_API_BASE", "https://api.stripe.com"),
		StripeAPIVersion:     envOr("STRIPE_API_VERSION", "2026-09-30.endive"),
		StripeWebhookSecret:  secretEnv("STRIPE_WEBHOOK_SECRET"),
		StripeSplitMetersBy:  envList("STRIPE_SPLIT_METERS_BY", []string{"sku"}),
		StripeAutoSubscribe:  envBool("STRIPE_AUTO_SUBSCRIBE", false),
		StripeMaxRPS:         envFloat("STRIPE_MAX_RPS", 0),
		StripeCancelOnRemove: envBool("STRIPE_CANCEL_ON_REMOVE", false),
		StripePriceCard:      os.Getenv("STRIPE_PRICE_CARD"),

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
	if c.RecordsPullLiteLLMURL != "" {
		c.RecordsLiteLLM = true // pulled rows are read with the litellm format
	}
	// Records are on time until Settle after their window; it must be
	// shorter than the window, so short windows get half the window.
	if c.RecordsSettle >= c.CollectionInterval {
		c.RecordsSettle = c.CollectionInterval / 2
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
	if c.EnforcementLiteLLM && (c.RecordsPullLiteLLMURL == "" || c.RecordsPullLiteLLMKey == "") {
		return fmt.Errorf("ENFORCEMENT_LITELLM needs RECORDS_PULL_LITELLM_URL and RECORDS_PULL_LITELLM_KEY (a LiteLLM admin key)")
	}
	if c.CollectionDelay < 0 || c.CollectionDelay > 10*time.Minute {
		return fmt.Errorf("COLLECTION_DELAY %s must be between 0 and 10m", c.CollectionDelay)
	}
	switch c.CPUMemoryBasis {
	case "usage", "requests", "max":
	default:
		return fmt.Errorf("CPU_MEMORY_BASIS %q must be usage, requests or max", c.CPUMemoryBasis)
	}
	switch c.TenantSource {
	case "cluster", "project":
	case "platform":
		if c.PlatformURL == "" || c.PlatformAccessKey == "" {
			return fmt.Errorf("TENANT_SOURCE=platform needs PLATFORM_URL and PLATFORM_ACCESS_KEY")
		}
		if c.PlatformCAFile != "" && c.PlatformInsecure {
			return fmt.Errorf("PLATFORM_CA_FILE and PLATFORM_INSECURE exclude each other")
		}
		if c.PlatformRefresh < 10*time.Second {
			return fmt.Errorf("PLATFORM_REFRESH %s must be at least 10s", c.PlatformRefresh)
		}
		if c.PlatformOrphanGrace < 0 {
			return fmt.Errorf("PLATFORM_ORPHAN_GRACE %s must not be negative", c.PlatformOrphanGrace)
		}
	default:
		return fmt.Errorf("TENANT_SOURCE %q must be cluster, project or platform", c.TenantSource)
	}
	switch c.EnforcementMode {
	case "observe", "annotate", "enforce":
	default:
		return fmt.Errorf("ENFORCEMENT_MODE %q must be observe, annotate or enforce", c.EnforcementMode)
	}
	if c.PauseDrainTimeout <= 0 {
		return fmt.Errorf("PAUSE_DRAIN_TIMEOUT %s must be positive", c.PauseDrainTimeout)
	}
	switch c.PrivateNodeBilling {
	case "node", "usage":
	default:
		return fmt.Errorf("PRIVATE_NODE_BILLING %q must be node or usage", c.PrivateNodeBilling)
	}
	if c.RecordsSettle <= 0 {
		return fmt.Errorf("RECORDS_SETTLE %s must be positive", c.RecordsSettle)
	}
	if c.RecordsArchive && c.ArchiveDir == "" {
		return fmt.Errorf("RECORDS_ARCHIVE needs ARCHIVE_DIR")
	}
	if c.RecordsRetention < 2*time.Hour {
		return fmt.Errorf("RECORDS_RETENTION %s must be at least 2h", c.RecordsRetention)
	}
	switch v := os.Getenv("RECORDS_UNKNOWN_TENANTS"); v {
	case "", "accept", "reject":
	default:
		return fmt.Errorf("RECORDS_UNKNOWN_TENANTS %q must be accept or reject", v)
	}
	for tok, tenant := range c.IngestTokens {
		if tok == "" || tenant == "" {
			return fmt.Errorf("INGEST_TOKENS entries must be token=tenant")
		}
	}
	if c.RecordsPullLiteLLMURL != "" {
		if c.RecordsPullLiteLLMKey == "" {
			return fmt.Errorf("RECORDS_PULL_LITELLM_URL needs RECORDS_PULL_LITELLM_KEY, a LiteLLM master or proxy admin key")
		}
		if c.RecordsPullLiteLLMInterval < 10*time.Second {
			return fmt.Errorf("RECORDS_PULL_LITELLM_INTERVAL %s must be at least 10s", c.RecordsPullLiteLLMInterval)
		}
		// Pulled records older than the records retention minus an hour are
		// refused, and late records are deduplicated against stored ones.
		if c.RecordsPullLiteLLMLookback < time.Minute || c.RecordsPullLiteLLMLookback > c.RecordsRetention-2*time.Hour {
			return fmt.Errorf("RECORDS_PULL_LITELLM_LOOKBACK %s must be between 1m and RECORDS_RETENTION minus 2h (%s)",
				c.RecordsPullLiteLLMLookback, c.RecordsRetention-2*time.Hour)
		}
	}
	switch c.PriceCheck {
	case "warn", "hold", "off":
	default:
		return fmt.Errorf("PRICE_CHECK %q must be warn, hold or off", c.PriceCheck)
	}
	if c.PriceCheckInterval < time.Minute {
		return fmt.Errorf("PRICE_CHECK_INTERVAL %s must be at least 1m", c.PriceCheckInterval)
	}
	if c.PriceCheckLookback < time.Hour {
		return fmt.Errorf("PRICE_CHECK_LOOKBACK %s must be at least 1h", c.PriceCheckLookback)
	}
	if c.PriceCheckMaxHold < time.Hour || c.PriceCheckMaxHold > MaxPriceHold {
		return fmt.Errorf("PRICE_CHECK_MAX_HOLD %s must be between 1h and %s: Metronome refuses usage older than 34 days, "+
			"and vBilling keeps a 2-day margin to deliver what it releases", c.PriceCheckMaxHold, MaxPriceHold)
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

// MaxPriceHold is the longest PRICE_CHECK_MAX_HOLD: the smallest backdating
// limit of a billing platform (Metronome, 34 days) minus a 2-day margin.
const MaxPriceHold = 32 * 24 * time.Hour

// envDays is envDuration that also accepts whole days ("28d"). An invalid
// value yields -1, which Validate rejects, instead of the default.
func envDays(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil {
			return time.Duration(d) * 24 * time.Hour
		}
		return -1
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return -1
	}
	return d
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

// secretMap parses "k1=v1,k2=v2" from a secret, trimming each part.
func secretMap(key string) map[string]string {
	out := map[string]string{}
	for k, v := range envMap(key) {
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
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
