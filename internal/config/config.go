// Package config loads vBilling settings from environment variables.
// Settings an earlier version read and v0.3.1 removed are listed in
// RemovedSettings: they are ignored with a warning.
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
	// (its tenant ID and aliases), using LITELLM_URL and LITELLM_API_KEY.
	EnforcementLiteLLM bool
	// PauseKeepBilling is the capacity that keeps billing while a cluster
	// is paused: storage, load_balancers, nodes, control_plane, workloads, or none.
	PauseKeepBilling string
	// PauseDrainTimeout bounds the wait for workloads on a cluster's own
	// nodes to stop before its control plane is stopped.
	PauseDrainTimeout time.Duration
	// PauseKeepNamespaces are the namespaces whose DaemonSets (GPU driver,
	// CNI) keep running on a paused cluster's own nodes; nil for the
	// default list, empty ("none") to stop every DaemonSet.
	PauseKeepNamespaces []string

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
	MetronomeStripeLink    bool   // create the Stripe customer and link it for invoicing (provision mode with STRIPE_API_KEY)
	// MetronomeProvision lets vBilling create billable metrics, customers and,
	// with METRONOME_RATE_CARD, contracts. Off (the default), vBilling only
	// sends usage keyed on the ingest alias and reads Metronome to check prices:
	// customers, metrics and contracts stay with whoever owns the account.
	MetronomeProvision     bool
	MetronomeStripeCollect string // charge_automatically | send_invoice

	// Webhook (CloudEvents).
	WebhookURL     string
	WebhookSecret  string
	WebhookHeaders map[string]string

	// Warnings name removed or renamed settings that are still set.
	Warnings []string
}

// Fixed values of settings v0.3.1 removed. Each one tuned an internal that
// has one right value, so a setting only added a way to get billing wrong.
const (
	windowSize          = 60 * time.Second
	maxBackfill         = 6 * time.Hour
	offboardGrace       = time.Hour
	reconcileInterval   = 30 * time.Second
	ledgerRetention     = 7 * 24 * time.Hour // live segments; the archive keeps compressed copies
	promLookback        = 10 * time.Minute
	recordsSettle       = 30 * time.Second
	recordsRetention    = 7 * 24 * time.Hour
	litellmPullInterval = 5 * time.Minute
	litellmPullLookback = time.Hour
	pauseDrainTimeout   = 2 * time.Minute
	priceCheckInterval  = 10 * time.Minute
	priceCheckLookback  = 24 * time.Hour
	priceCheckMaxHold   = 28 * 24 * time.Hour
	stripeAPIVersion    = "2026-09-30.endive"
	tenantSecret        = "vbilling-kubeconfig"
	platformRefresh     = time.Minute
)

// RemovedSettings are the environment variables earlier versions read. A
// value still set only logs a warning naming what replaced it, so an
// upgrade never fails on old Helm values or manifests.
var RemovedSettings = map[string]string{
	"ADAPTER":                            "use ADAPTERS",
	"COLLECTION_INTERVAL":                "windows are always 60s",
	"RECONCILE_INTERVAL":                 "fixed at 30s",
	"MAX_BACKFILL":                       "fixed at 6h",
	"OFFBOARD_GRACE":                     "fixed at 1h",
	"RETENTION":                          "live segments are kept 7 days; ARCHIVE_DIR keeps compressed copies",
	"ZONE":                               "the zone comes from each node's topology.kubernetes.io/zone label",
	"DEFAULT_TENANT_CLASS":               "set vbilling.vcluster.com/tenant-class on the tenant",
	"REGION_FROM_NODE":                   "REGION is stamped on every event; run one vBilling per region",
	"WATCH_DELETIONS":                    "deleted pods and nodes are always billed to the second",
	"DRA_GPU_DRIVERS":                    "gpu.nvidia.com, gpu.amd.com and gpu.intel.com are GPUs",
	"TENANT_API":                         "clusters with their own nodes are always metered through their API",
	"TENANT_KUBECONFIG_SECRET":           "the Secret is always vbilling-kubeconfig",
	"TENANT_ADMIN_FALLBACK":              "removed: vBilling never uses a cluster's admin kubeconfig",
	"TENANT_EXCLUDE_NAMESPACES":          "label a namespace vbilling.vcluster.com/exclude=true",
	"PLATFORM_PROXY":                     "with TENANT_SOURCE=platform, clusters are always reached through the Platform proxy",
	"PLATFORM_REFRESH":                   "fixed at 1m",
	"PLATFORM_ORPHAN_GRACE":              "removed",
	"PROMETHEUS_LOOKBACK":                "fixed at 10m",
	"RECORDS_SETTLE":                     "fixed at 30s",
	"RECORDS_RETENTION":                  "fixed at 7 days",
	"RECORDS_ALLOW_UNKEYED":              "removed: every record needs a request ID",
	"RECORDS_ARCHIVE":                    "records are archived whenever ARCHIVE_DIR is on",
	"RECORDS_PULL_LITELLM_INTERVAL":      "fixed at 5m",
	"RECORDS_PULL_LITELLM_LOOKBACK":      "fixed at 1h",
	"PAUSE_DRAIN_TIMEOUT":                "fixed at 2m",
	"PRICE_CHECK_INTERVAL":               "fixed at 10m",
	"PRICE_CHECK_LOOKBACK":               "fixed at 24h",
	"PRICE_CHECK_MAX_HOLD":               "fixed at 28 days",
	"STRIPE_API_VERSION":                 "pinned by vBilling",
	"STRIPE_MAX_RPS":                     "vBilling paces Stripe requests itself",
	"STRIPE_CANCEL_ON_REMOVE":            "removed: end subscriptions in Stripe",
	"METRONOME_STRIPE_LINK":              "METRONOME_PROVISION=true links a Stripe customer when STRIPE_API_KEY is set",
	"METRONOME_STRIPE_COLLECTION_METHOD": "fixed at charge_automatically",
	"SPOT_DISCOUNT_PERCENT":              "removed in v0.2: price capacity_type in your billing platform",
}

// renamedSettings map an old name to its replacement; the old value is used
// when only the old name is set.
var renamedSettings = map[string]string{
	"RECORDS_PULL_LITELLM_URL":    "LITELLM_URL",
	"RECORDS_PULL_LITELLM_KEY":    "LITELLM_API_KEY",
	"RECORDS_PULL_LITELLM_CLIENT": "LITELLM_CLIENT",
	"RECORDS_PULL_LITELLM_TENANT": "LITELLM_TENANT",
}

func Load() *Config {
	var warnings []string
	for old, repl := range renamedSettings {
		if os.Getenv(old) != "" {
			warnings = append(warnings, fmt.Sprintf("%s is now %s", old, repl))
		}
	}
	for k, why := range RemovedSettings {
		if os.Getenv(k) != "" {
			warnings = append(warnings, fmt.Sprintf("%s is no longer read: %s", k, why))
		}
	}
	c := &Config{
		Adapters:    envList("ADAPTERS", nil),
		Region:      envOr("REGION", "default"),
		ClusterName: envOr("CLUSTER_NAME", "control-plane"),

		DataDir:    envOr("DATA_DIR", "/var/lib/vbilling"),
		Retention:  ledgerRetention,
		ArchiveDir: os.Getenv("ARCHIVE_DIR"),

		CollectionInterval: windowSize,
		ReconcileInterval:  reconcileInterval,
		MaxBackfill:        maxBackfill,
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
		DRAGPUDrivers:      []string{"gpu.nvidia.com", "gpu.amd.com", "gpu.intel.com"},
		WatchDeletions:     true,
		PromSourceLabel:    envOr("PROMETHEUS_SOURCE_LABEL", "vcluster"),
		PromLookback:       promLookback,

		TenantAPI:               true,
		TenantKubeconfigSecret:  tenantSecret,
		TenantClustersFile:      os.Getenv("TENANT_CLUSTERS_FILE"),
		PrivateNodeBilling:      envOr("PRIVATE_NODE_BILLING", "node"),
		TenantExcludeNamespaces: []string{"kube-system"},
		WatchNamespaces:         envList("WATCH_NAMESPACES", nil),
		DedicatedNodeLabel:      os.Getenv("VCLUSTER_NODE_LABEL"),
		TenantSource:            envOr("TENANT_SOURCE", "cluster"),
		PlatformCluster:         os.Getenv("PLATFORM_CLUSTER"),
		PlatformURL:             os.Getenv("PLATFORM_URL"),
		PlatformAccessKey:       secretEnv("PLATFORM_ACCESS_KEY"),
		PlatformCAFile:          os.Getenv("PLATFORM_CA_FILE"),
		PlatformInsecure:        envBool("PLATFORM_INSECURE", false),
		PlatformRefresh:         platformRefresh,
		PlatformProxy:           true,
		OffboardGrace:           offboardGrace,
		CustomMetrics:           os.Getenv("CUSTOM_METRICS"), // v0.2 string form, still accepted
		CustomMetricsFile:       os.Getenv("CUSTOM_METRICS_FILE"),
		PlanByClass:             envMap("PLAN_BY_CLASS"),
		CollectionDelay:         envDuration("COLLECTION_DELAY", 5*time.Second),
		RecordsSettle:           recordsSettle,
		RecordsRetention:        recordsRetention,
		RecordsMappingsFile:     os.Getenv("RECORDS_MAPPINGS_FILE"),
		RecordsRejectUnknown:    os.Getenv("RECORDS_UNKNOWN_TENANTS") == "reject",
		RecordsLiteLLM:          envBool("RECORDS_LITELLM", false),
		IngestTokens:            secretMap("INGEST_TOKENS"),
		CrossChecks:             os.Getenv("CROSS_CHECKS"),

		RecordsPullLiteLLMURL:      renamed("LITELLM_URL"),
		RecordsPullFile:            os.Getenv("RECORDS_PULL_FILE"),
		RecordsPullLiteLLMKey:      strings.TrimSpace(renamed("LITELLM_API_KEY")),
		RecordsPullLiteLLMClient:   orDefault(renamed("LITELLM_CLIENT"), "litellm"),
		RecordsPullLiteLLMTenant:   renamed("LITELLM_TENANT"),
		RecordsPullLiteLLMInterval: litellmPullInterval,
		RecordsPullLiteLLMLookback: litellmPullLookback,

		DefaultPlanCode: envOr("DEFAULT_PLAN_CODE", "vcluster-standard"),
		BillingCurrency: envOr("BILLING_CURRENCY", "USD"),

		PriceCheck:         envOr("PRICE_CHECK", "warn"),
		PriceCheckInterval: priceCheckInterval,
		PriceCheckLookback: priceCheckLookback,
		PriceCheckMaxHold:  priceCheckMaxHold,

		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		APIToken:    secretEnv("API_TOKEN"),
		IngestToken: secretEnv("INGEST_TOKEN"),

		EnforcementMode:      envOr("ENFORCEMENT_MODE", "observe"),
		EnforcementRules:     os.Getenv("ENFORCEMENT_RULES"),
		EnforcementInCluster: envBool("ENFORCEMENT_IN_CLUSTER", false),
		EnforcementLiteLLM:   envBool("ENFORCEMENT_LITELLM", false),
		PauseKeepBilling:     envOr("PAUSE_KEEP_BILLING", "storage,load_balancers"),
		PauseDrainTimeout:    pauseDrainTimeout,
		PauseKeepNamespaces:  envList("PAUSE_KEEP_NAMESPACES", nil), // nil: enforcement.DefaultKeepNamespaces

		LagoAPIURL:       envOr("LAGO_API_URL", "http://localhost:3000"),
		LagoAPIKey:       secretEnv("LAGO_API_KEY"),
		LagoPriceCard:    os.Getenv("LAGO_PRICE_CARD"),
		LagoManagedPlans: envList("LAGO_MANAGED_PLANS", nil),

		StripeAPIKey:        secretEnv("STRIPE_API_KEY"),
		StripeAPIBase:       envOr("STRIPE_API_BASE", "https://api.stripe.com"), // tests: stripe-mock
		StripeAPIVersion:    stripeAPIVersion,
		StripeWebhookSecret: secretEnv("STRIPE_WEBHOOK_SECRET"),
		StripeSplitMetersBy: envList("STRIPE_SPLIT_METERS_BY", []string{"sku"}),
		StripeAutoSubscribe: envBool("STRIPE_AUTO_SUBSCRIBE", false),
		StripePriceCard:     os.Getenv("STRIPE_PRICE_CARD"),

		MetronomeAPIToken:      secretEnv("METRONOME_API_TOKEN"),
		MetronomeAPIBase:       envOr("METRONOME_API_BASE", "https://api.metronome.com"), // tests: a fake server
		MetronomeWebhookSecret: secretEnv("METRONOME_WEBHOOK_SECRET"),
		MetronomeRateCard:      os.Getenv("METRONOME_RATE_CARD"),
		MetronomeProvision:     envBool("METRONOME_PROVISION", false),
		MetronomeStripeCollect: "charge_automatically",

		WebhookURL:     os.Getenv("WEBHOOK_URL"),
		WebhookSecret:  secretEnv("WEBHOOK_SECRET"),
		WebhookHeaders: envMap("WEBHOOK_HEADERS"),
	}
	if len(c.Adapters) == 0 {
		c.Adapters = []string{envOr("ADAPTER", "lago")}
	}
	if envBool("METRONOME_STRIPE_LINK", false) {
		c.MetronomeProvision = true // v0.3.0: the link implied creating customers
	}
	c.MetronomeStripeLink = c.MetronomeProvision && c.StripeAPIKey != ""
	if c.MetronomeRateCard != "" && !c.MetronomeProvision {
		warnings = append(warnings, "METRONOME_RATE_CARD only creates contracts with METRONOME_PROVISION=true; price checks read each customer's own contract")
	}
	if c.ArchiveDir == "off" {
		c.ArchiveDir = "" // v0.3.1 pre-releases documented "off"
	}
	c.RecordsArchive = c.ArchiveDir != ""
	if c.IngestToken == "" {
		c.IngestToken = c.APIToken
	}
	if c.RecordsPullLiteLLMURL != "" {
		c.RecordsLiteLLM = true // pulled rows are read with the litellm format
	}
	sortStrings(warnings)
	c.Warnings = warnings
	return c
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// renamed reads a setting, falling back to its old name.
func renamed(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	for old, repl := range renamedSettings {
		if repl == key {
			return os.Getenv(old)
		}
	}
	return ""
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
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
		return fmt.Errorf("ENFORCEMENT_LITELLM needs LITELLM_URL and LITELLM_API_KEY (a LiteLLM admin key)")
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
		// INGEST_TOKEN is unset here only when API_TOKEN is too; Load
		// fills it with API_TOKEN otherwise, so this also covers that one.
		if c.IngestToken != "" && tok == c.IngestToken {
			return fmt.Errorf("the INGEST_TOKENS token of tenant %s is also the shared ingest token (INGEST_TOKEN, or API_TOKEN when it is unset): "+
				"give each tenant its own token", tenant)
		}
	}
	// The token map keeps the last tenant of a token listed twice, so read
	// the list as written: one token for two tenants would bill everything
	// the first tenant's gateway sends to the second.
	owner := map[string]string{}
	for _, kv := range envList("INGEST_TOKENS", nil) {
		tok, tenant, _ := strings.Cut(kv, "=")
		tok, tenant = strings.TrimSpace(tok), strings.TrimSpace(tenant)
		if prev, dup := owner[tok]; dup && tok != "" {
			if prev == tenant {
				return fmt.Errorf("INGEST_TOKENS lists the token of tenant %s twice", tenant)
			}
			return fmt.Errorf("INGEST_TOKENS gives one token to tenants %s and %s: give each tenant its own token", prev, tenant)
		}
		owner[tok] = tenant
	}
	// Gateways hold the shared ingest token in their configuration; the API
	// token pauses tenants and exports everyone's usage. Without
	// INGEST_TOKEN, vBilling generates a separate one (DATA_DIR/ingest-token).
	if ing := secretEnv("INGEST_TOKEN"); ing != "" && ing == c.APIToken {
		return fmt.Errorf("INGEST_TOKEN must not be the API token: gateways hold the ingest token, and the API token pauses tenants and exports all usage. " +
			"Unset INGEST_TOKEN to have vBilling generate one in DATA_DIR/ingest-token")
	}
	if c.RecordsPullLiteLLMURL != "" {
		if c.RecordsPullLiteLLMKey == "" {
			return fmt.Errorf("LITELLM_URL needs LITELLM_API_KEY, a LiteLLM master or proxy admin key")
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
