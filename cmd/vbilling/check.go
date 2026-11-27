package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/api"
	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/crosscheck"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/platform"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/tenantapi"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
	vbversion "github.com/vclusterlabs-experiments/vbilling/internal/version"
)

// The configuration is read and checked in one place, loadStartupConfig,
// which both start-up (main) and "vbilling check-config" call. vBilling runs
// as one instance, so a configuration it refuses at start-up means nothing
// meters until someone reverts it; check-config lets the chart's pre-install
// and pre-upgrade Job refuse it while the running pod keeps metering.
//
// Every check of a setting or of a file a setting names belongs here, not in
// main: what only main checks, check-config cannot see. check_test.go fails
// when main stops on a setting this file does not check.

// startupConfig is the configuration vBilling starts with: the settings in
// the environment and what was parsed from the files they name.
type startupConfig struct {
	cfg         *config.Config
	formats     records.Formats
	promMetrics []metrics.PromMetric
	allowances  []metrics.Allowance // ALLOWANCES_FILE

	// checked lists what was read and checked, in order.
	checked []string
	// warnings never stop vBilling: removed or renamed settings that are
	// still set and, with allowMissingSecrets, credentials that are not there.
	warnings []string
}

// startupOptions tell loadStartupConfig where it runs.
type startupOptions struct {
	// allowMissingSecrets is for a check that runs before the Secrets of the
	// installation exist (the chart's pre-install Job): a credential that is
	// not set, and a file from a Secret that is not mounted, is a warning,
	// and the configuration that depends on it is checked as far as it can
	// be. Everything else stays an error. Start-up never sets it.
	allowMissingSecrets bool
	// scratch is an empty directory that stands in for DATA_DIR in the
	// checks start-up makes only once it holds the Lease and has opened its
	// data: the billing adapters (their settings and price cards) and the
	// API tokens. Empty at start-up, which makes those checks for real.
	scratch string
}

func (st *startupConfig) ok(format string, args ...any) {
	st.checked = append(st.checked, fmt.Sprintf(format, args...))
}

func (st *startupConfig) warn(format string, args ...any) {
	st.warnings = append(st.warnings, fmt.Sprintf(format, args...))
}

// loadStartupConfig reads the settings from the environment and every file
// they name, and checks them. The error is the one start-up stops with. It
// contacts nothing (no Kubernetes API, billing platform, Prometheus or
// gateway), takes no Lease and never opens DATA_DIR or ARCHIVE_DIR.
//
// The returned configuration is never nil: it carries the warnings and what
// was checked up to the error. Call it once per process: it declares the
// custom metrics in the process-wide catalog.
func loadStartupConfig(opts startupOptions) (*startupConfig, error) {
	st := &startupConfig{cfg: config.Load()}
	for _, w := range st.cfg.Warnings {
		st.warn("setting %s", w)
	}
	return st, st.load(opts)
}

func (st *startupConfig) load(opts startupOptions) error {
	cfg := st.cfg
	if opts.allowMissingSecrets {
		for _, c := range requiredCredentials(cfg) {
			if c.needed && *c.value == "" {
				*c.value = "not-set" // never sent anywhere: nothing is contacted
				st.warn("%s is not set here, and %s needs it: it is not checked, and vBilling does not start without it", c.env, c.by)
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	st.ok("settings: adapters %s, region %s, tenant source %s, enforcement %s, price check %s",
		strings.Join(cfg.Adapters, ","), cfg.Region, cfg.TenantSource, cfg.EnforcementMode, cfg.PriceCheck)

	// The metric catalog, in this order: a later source may not redeclare a
	// metric an earlier one declared. PRICING_KEYS names built-in metrics
	// only, which Validate checked.
	if err := declarePricingKeys(cfg); err != nil {
		return err
	}
	if keys, _ := usage.ParsePricingKeys(cfg.PricingKeys); len(keys) > 0 {
		st.ok("PRICING_KEYS: %s", usage.FormatPricingKeys(keys))
	}
	customs, err := usage.ParseCustomMetrics(cfg.CustomMetrics)
	if err != nil {
		return fmt.Errorf("CUSTOM_METRICS: %w", err)
	}
	if cfg.CustomMetrics != "" {
		st.ok("CUSTOM_METRICS: %d metric(s)", len(customs))
	}
	if cfg.CustomMetricsFile != "" {
		b, err := os.ReadFile(cfg.CustomMetricsFile)
		if err != nil {
			return fmt.Errorf("CUSTOM_METRICS_FILE: %w", err)
		}
		more, err := usage.ParseCustomMetricsYAML(b)
		if err != nil {
			return fmt.Errorf("CUSTOM_METRICS_FILE %s: %w", cfg.CustomMetricsFile, err)
		}
		customs = append(customs, more...)
		st.ok("CUSTOM_METRICS_FILE %s: %d metric(s)", cfg.CustomMetricsFile, len(more))
	}
	for _, d := range customs {
		if err := usage.RegisterCustom(d); err != nil {
			return fmt.Errorf("CUSTOM_METRICS: %w", err)
		}
	}
	st.formats, err = records.LoadFormats(cfg.RecordsMappingsFile, cfg.RecordsLiteLLM)
	if err != nil {
		return fmt.Errorf("RECORDS_MAPPINGS_FILE: %w", err)
	}
	if err := st.formats.Declare(); err != nil {
		return fmt.Errorf("records formats: %w", err)
	}
	if cfg.RecordsMappingsFile != "" {
		st.ok("RECORDS_MAPPINGS_FILE %s: %d records format(s) with the built-in ones", cfg.RecordsMappingsFile, len(st.formats))
	}
	// Egress is a counter (exact) unless EGRESS_QUERY replaces it.
	if cfg.EgressQuery != "" && cfg.EgressQuery != "none" {
		usage.SetAccuracy(usage.MetricNetworkEgressGB, usage.AccuracyEstimate)
	}
	// A source that is switched off takes its metric out of the catalog.
	usage.SetEnabled(usage.MetricNetworkEgressGB, cfg.EgressQuery != "none")
	usage.SetEnabled(usage.MetricGPUUtilization, cfg.GPUUtilQuery != "none")
	if cfg.CPUMemoryBasis != "requests" {
		// Usage is a metrics-server sample of the live window, not a
		// measurement over it: requests is the exact basis.
		usage.SetAccuracy(usage.MetricCPUCoreHours, usage.AccuracyEstimate)
		usage.SetAccuracy(usage.MetricMemoryGBHours, usage.AccuracyEstimate)
	}
	st.promMetrics, err = metrics.LoadPromMetrics(cfg.PrometheusMetrics)
	if err != nil {
		return fmt.Errorf("PROMETHEUS_METRICS_FILE: %w", err)
	}
	if cfg.PrometheusMetrics != "" {
		st.ok("PROMETHEUS_METRICS_FILE %s: %d metric(s)", cfg.PrometheusMetrics, len(st.promMetrics))
	}
	if err := st.loadAllowances(); err != nil {
		return err
	}

	// vCluster Platform (TENANT_SOURCE=platform): the URL, the access key
	// and the CA file. The client is built, never used.
	if cfg.TenantSource == "platform" {
		po := platformClientOptions(cfg)
		if opts.allowMissingSecrets && missingFile(po.CAFile) {
			st.warn("PLATFORM_CA_FILE %s is not there (it is mounted from a Secret): not checked here", po.CAFile)
			po.CAFile = ""
		}
		if _, err := platform.NewClient(po); err != nil {
			return fmt.Errorf("vCluster Platform: %w", err)
		}
		ca := ""
		if po.CAFile != "" {
			ca = ", CA " + po.CAFile
		}
		st.ok("vCluster Platform: %s%s", cfg.PlatformURL, ca)
	}

	keep, err := pause.ParseKeep(cfg.PauseKeepBilling)
	if err != nil {
		return err // names PAUSE_KEEP_BILLING itself
	}
	st.ok("PAUSE_KEEP_BILLING: %s", keep)

	if cfg.EnforcementInCluster && !cfg.TenantAPI {
		return errors.New("ENFORCEMENT_IN_CLUSTER needs TENANT_API=true")
	}
	if cfg.TenantAPI {
		ext, err := tenantapi.LoadExternal(cfg.TenantClustersFile)
		switch {
		case err != nil && opts.allowMissingSecrets && errors.Is(err, fs.ErrNotExist):
			st.warn("TENANT_CLUSTERS_FILE %s is not there (it is mounted from a Secret): not checked here", cfg.TenantClustersFile)
		case err != nil:
			return fmt.Errorf("TENANT_CLUSTERS_FILE: %w", err)
		case cfg.TenantClustersFile != "":
			st.ok("TENANT_CLUSTERS_FILE %s: %d cluster(s)", cfg.TenantClustersFile, len(ext))
		}
	}

	// Cross-checks name metrics: after the catalog is complete.
	checks, err := crosscheck.Parse(cfg.CrossChecks)
	if err != nil {
		return fmt.Errorf("CROSS_CHECKS: %w", err)
	}
	if cfg.CrossChecks != "" {
		st.ok("CROSS_CHECKS: %d check(s)", len(checks))
	}

	// Enforcement: everything it reads from the configuration (mode, rules,
	// gateway and pause settings) is checked by its own validator, here, so
	// that start-up and check-config both run it before the Lease is taken.
	if err := enforcement.ValidateConfig(cfg); err != nil {
		return err
	}
	// The validator parsed the rules already; parsed again here for their
	// count, with the text start-up stops with where it builds the enforcer.
	rules, err := enforcement.ParseRules(cfg.EnforcementRules)
	if err != nil {
		return fmt.Errorf("ENFORCEMENT_RULES: %w", err)
	}
	if cfg.EnforcementRules != "" {
		st.ok("ENFORCEMENT_RULES: %d rule(s)", len(rules))
	}
	escalations, err := enforcement.ParseEscalations(cfg.EnforcementEscalations)
	if err != nil {
		return fmt.Errorf("ENFORCEMENT_ESCALATIONS: %w", err)
	}
	if len(escalations) > 0 {
		names := make([]string, len(escalations))
		for i, r := range escalations {
			names[i] = r.String()
		}
		st.ok("ENFORCEMENT_ESCALATIONS: %s", strings.Join(names, ", "))
	}

	pulls, err := records.LoadPullSpecs(cfg.RecordsPullFile, st.formats)
	if err != nil {
		return fmt.Errorf("RECORDS_PULL_FILE: %w", err)
	}
	if len(pulls) > 0 {
		// An ingester without a ledger: only what it knows of INGEST_TOKENS
		// is used, to check the pulls pinned to a tenant.
		pins := records.NewIngester(nil, st.formats, records.Options{BoundTokens: cfg.IngestTokens}, nil, nil, nil)
		for _, spec := range pulls {
			if lb := time.Duration(spec.Lookback); lb > cfg.RecordsRetention-2*time.Hour {
				return fmt.Errorf("RECORDS_PULL_FILE: pull %s: lookback %s must be at most RECORDS_RETENTION minus 2h (%s)", spec.Name, lb, cfg.RecordsRetention-2*time.Hour)
			}
			if err := pins.CheckPin(spec.Tenant); err != nil {
				return fmt.Errorf("RECORDS_PULL_FILE: pull %s: %w", spec.Name, err)
			}
		}
	}
	if cfg.RecordsPullFile != "" {
		st.ok("RECORDS_PULL_FILE %s: %d pull(s)", cfg.RecordsPullFile, len(pulls))
	}

	if opts.scratch == "" {
		return nil // start-up: the adapters and the tokens are set up for real, with the Lease held
	}
	// What start-up checks with its data open, here against an empty
	// directory: the same constructors, nothing of DATA_DIR, and nothing
	// written. They come last, as at start-up, so the first error is the
	// one start-up stops with.
	data := *cfg
	data.DataDir = opts.scratch
	if _, err := destinations.NewAll(data.Adapters, &data); err != nil {
		return fmt.Errorf("initialize adapters: %w", err)
	}
	st.ok("adapters %s: settings%s", strings.Join(data.Adapters, ","), priceCards(cfg))
	// A token that is not set is generated in DATA_DIR at start-up (the
	// shared ingest token also when it is the API token). Random stand-ins
	// here, so the tokens that are set are compared without a file written.
	if data.APIToken == "" {
		data.APIToken = standInToken()
	}
	if data.IngestToken == "" || data.IngestToken == data.APIToken {
		data.IngestToken = standInToken()
	}
	if _, err := api.PrepareTokens(&data); err != nil {
		return fmt.Errorf("API token: %w", err)
	}
	st.ok("ingest tokens: %d bound to a tenant, none of them the API token or the shared ingest token", len(cfg.IngestTokens))
	return nil
}

// loadAllowances reads ALLOWANCES_FILE. With an allowance, storage is
// billed as vcluster_storage_overage_gb_hours, and vcluster_storage_gb_hours
// stays in the ledger as information.
func (st *startupConfig) loadAllowances() error {
	cfg := st.cfg
	allowances, err := metrics.LoadAllowances(cfg.AllowancesFile)
	if err != nil {
		return fmt.Errorf("ALLOWANCES_FILE: %w", err)
	}
	st.allowances = allowances
	usage.UseStorageAllowances(len(allowances) > 0)
	if cfg.AllowancesFile == "" {
		return nil
	}
	if len(allowances) == 0 {
		st.ok("ALLOWANCES_FILE %s: no allowances: %s is billed", cfg.AllowancesFile, usage.MetricStorageGBHours)
		return nil
	}
	st.ok("ALLOWANCES_FILE %s: %d allowance(s): storage is billed as %s, the storage above them; %s is informational",
		cfg.AllowancesFile, len(allowances), usage.MetricStorageOverageGBHours, usage.MetricStorageGBHours)
	for _, a := range allowances {
		if a.Per == metrics.PerNode && cfg.PrivateNodeBilling == "usage" {
			st.warn("ALLOWANCES_FILE: an allowance per node counts the nodes billed whole, and PRIVATE_NODE_BILLING=usage bills the pods on a cluster's own nodes instead: " +
				"only dedicated nodes include storage then, and the storage of clusters with their own nodes is all above the allowance")
			break
		}
	}
	return nil
}

// credential is a secret setting that part of the configuration requires.
type credential struct {
	env    string
	value  *string
	needed bool
	by     string // what needs it
}

// requiredCredentials lists the credentials vBilling refuses to start
// without, and whether cfg needs each. An adapter that requires a new one
// adds it here (check_test.go runs every registered adapter without any).
func requiredCredentials(cfg *config.Config) []credential {
	adapter := func(name string) bool { return slices.Contains(cfg.Adapters, name) }
	return []credential{
		{"LAGO_API_KEY", &cfg.LagoAPIKey, adapter("lago"), "the lago adapter"},
		{"STRIPE_API_KEY", &cfg.StripeAPIKey, adapter("stripe"), "the stripe adapter"},
		{"METRONOME_API_TOKEN", &cfg.MetronomeAPIToken, adapter("metronome"), "the metronome adapter"},
		{"PLATFORM_ACCESS_KEY", &cfg.PlatformAccessKey, cfg.TenantSource == "platform", "TENANT_SOURCE=platform"},
		{"LITELLM_API_KEY", &cfg.RecordsPullLiteLLMKey, cfg.RecordsPullLiteLLMURL != "", "LITELLM_URL"},
	}
}

// platformClientOptions are the settings of the vCluster Platform client
// (TENANT_SOURCE=platform), the same at start-up and in check-config.
func platformClientOptions(cfg *config.Config) platform.ClientOptions {
	return platform.ClientOptions{
		URL:       cfg.PlatformURL,
		AccessKey: cfg.PlatformAccessKey,
		CAFile:    cfg.PlatformCAFile,
		Insecure:  cfg.PlatformInsecure,
		UserAgent: vbversion.UserAgent(),
	}
}

// standInToken is a random token nobody holds.
func standInToken() string {
	raw := make([]byte, 32)
	rand.Read(raw) // never fails (crypto/rand)
	return hex.EncodeToString(raw)
}

// scratchDir returns a directory without vBilling data in it, to stand in
// for DATA_DIR, and what removes it again. Nothing is written to it: the
// adapters only look for their state there. A new temporary directory when
// one can be made. On a read-only filesystem (kubectl exec into the
// vBilling pod) a directory that is there and never holds that state: the
// temporary directory itself, or the root directory in an image without one.
func scratchDir() (dir string, remove func(), err error) {
	dir, err = os.MkdirTemp("", "vbilling-check-config-")
	if err == nil {
		return dir, func() { os.RemoveAll(dir) }, nil
	}
	for _, existing := range []string{os.TempDir(), string(filepath.Separator)} {
		if info, statErr := os.Stat(existing); statErr == nil && info.IsDir() {
			return existing, func() {}, nil
		}
	}
	return "", nil, err
}

// missingFile reports whether a path is set and nothing is there.
func missingFile(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// priceCards names the price card files the configured adapters read.
func priceCards(cfg *config.Config) string {
	var cards []string
	if slices.Contains(cfg.Adapters, "stripe") && cfg.StripePriceCard != "" {
		cards = append(cards, "STRIPE_PRICE_CARD "+cfg.StripePriceCard)
	}
	if slices.Contains(cfg.Adapters, "lago") && cfg.LagoPriceCard != "" {
		cards = append(cards, "LAGO_PRICE_CARD "+cfg.LagoPriceCard)
	}
	if len(cards) == 0 {
		return ""
	}
	return ", " + strings.Join(cards, ", ")
}

const checkConfigUsage = `Usage: vbilling check-config [--allow-missing-secrets]

Reads and checks the configuration exactly as start-up does: the settings in
the environment and every file they name (custom metrics, Prometheus metric
definitions, storage allowances, records formats and pulls, price cards, the
tenant clusters file, the vCluster Platform CA). Exits 0 and lists what it
checked, or exits 1 with the error vBilling would stop with at start-up.

It contacts nothing (no Kubernetes API, billing platform, Prometheus or
gateway), takes no Lease, and never opens the data or archive volume. Run it
with the environment and the files of the vBilling pod. The chart does, with
the new configuration, in a Job before every install and upgrade
(configCheck.enabled), so a configuration vBilling would refuse fails the
upgrade and the running pod keeps metering.

`

// runCheckConfigCmd runs "vbilling check-config ..." and returns the exit code.
func runCheckConfigCmd(args []string) int {
	return checkConfigMain(args, os.Stdout, os.Stderr)
}

func checkConfigMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vbilling check-config", flag.ContinueOnError)
	flags.SetOutput(stderr)
	allowMissing := flags.Bool("allow-missing-secrets", false,
		"a credential that is not set, or a file from a Secret that is not mounted, is a warning, not an error: "+
			"for a check that runs before the installation's Secrets exist. Everything else stays an error")
	flags.Usage = func() {
		fmt.Fprint(stderr, checkConfigUsage)
		flags.PrintDefaults()
	}
	if code, done := parseFlags(flags, args, stderr); done {
		return code
	}
	scratch, remove, err := scratchDir()
	if err != nil {
		fmt.Fprintf(stderr, "check-config found no directory to stand in for DATA_DIR (set TMPDIR to an existing one): %v\n", err)
		return 1
	}
	defer remove()

	// What the adapters log when they are set up is part of the report.
	var logged bytes.Buffer
	logFlags, logOutput := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(&logged)
	defer func() {
		log.SetFlags(logFlags)
		log.SetOutput(logOutput)
	}()

	st, err := loadStartupConfig(startupOptions{allowMissingSecrets: *allowMissing, scratch: scratch})
	// The whole report goes to one stream, standard error when the check
	// fails: a pod's log merges the two streams in no particular order.
	out := stdout
	if err != nil {
		out = stderr
	}
	fmt.Fprintf(out, "vBilling %s: checking the configuration as start-up reads it\n", vbversion.Version)
	for _, line := range st.checked {
		fmt.Fprintf(out, "ok       %s\n", line)
	}
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if line != "" {
			fmt.Fprintf(out, "note     %s\n", line)
		}
	}
	for _, w := range st.warnings {
		fmt.Fprintf(out, "warning  %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(out, "error    %v\n", err)
		fmt.Fprintln(out, "The configuration is not valid: vBilling would stop at start-up with this error. Nothing was changed.")
		return 1
	}
	fmt.Fprintf(out, "The configuration is valid: %d check(s) passed, %d warning(s).\n", len(st.checked), len(st.warnings))
	return 0
}
