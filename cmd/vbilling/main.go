package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	"github.com/vclusterlabs-experiments/vbilling/internal/crosscheck"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/diskspace"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/gateway"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/lease"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/platform"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/safeguard"
	"github.com/vclusterlabs-experiments/vbilling/internal/sourceid"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/tenantapi"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
	vbversion "github.com/vclusterlabs-experiments/vbilling/internal/version"

	// Register the remaining built-in adapters.
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/lago"
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/noop"
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/webhook"
)

// version is written once, in internal/version: the log, the API and the
// User-Agent of every outgoing client report the same one.
var version = vbversion.Version

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ledger" {
		os.Exit(runLedgerCmd(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "metronome" {
		os.Exit(runMetronomeCmd(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "check-config" {
		os.Exit(runCheckConfigCmd(os.Args[2:]))
	}
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("vBilling %s: usage metering for tenant clusters", version)

	// Every check of the configuration is in loadStartupConfig (check.go),
	// which "vbilling check-config" runs too: add new ones there, so the
	// chart's Job refuses a bad configuration before the pod is replaced.
	// The parsers further down see settings that already passed.
	startup, err := loadStartupConfig(startupOptions{})
	for _, w := range startup.warnings {
		log.Printf("WARNING: %s", w)
	}
	if err != nil {
		log.Fatalf("%v", err)
	}
	cfg, formats, promMetrics := startup.cfg, startup.formats, startup.promMetrics
	log.Printf("Adapters: %v | Region: %s | Window: %s | CPU/memory basis: %s | Data: %s",
		cfg.Adapters, cfg.Region, cfg.CollectionInterval, cfg.CPUMemoryBasis, cfg.DataDir)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	kubeConfig, namespace, err := getKubeConfig()
	if err != nil {
		log.Fatalf("kubernetes config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	// One instance at a time: nothing is opened, metered, taken in or
	// delivered before this process holds the Lease.
	guard := holdLease(ctx, kube, namespace, cfg.ListenAddr)
	defer guard.Release()
	// A start-up error from here on ends the process with log.Fatal, which
	// skips the deferred release: give the Lease up first (see fatal.go).
	startupOver := releaseOnFatal(guard.Release)
	// The live second copy of the data directory, on the archive volume
	// (LEDGER_MIRROR). Where none is kept, an earlier copy is never restored
	// by itself: nothing says how old it is.
	mirrorPlan := spool.PlanMirror(cfg.LedgerMirror, cfg.ArchiveDir)
	mirrorDir := mirrorPlan.Dir
	if mirrorPlan.Reason != "" {
		log.Printf("[mirror] %s. %s", mirrorPlan.Reason, mirrorPlan.Hint)
		if mirrorPlan.Unused != "" {
			log.Printf("[mirror] WARNING: %s holds a copy from when the mirror was kept. It is not updated any more and vBilling never restores it by itself: delete it, or restore it by hand with vbilling ledger restore", mirrorPlan.Unused)
		}
	}
	// A data volume that was lost: DATA_DIR holds no ledger, but this pod's
	// mirror on the archive volume does. It comes back before anything is
	// opened, metering source identity included, so vBilling continues the
	// ledger instead of starting a new one next to its copy.
	if _, err := spool.RestoreIfLost(mirrorDir, cfg.DataDir, log.Printf); err != nil {
		log.Fatalf("restore the ledger: %s holds none and its mirror in %s could not be restored: %v. vBilling does not start as a new install next to the copy of a ledger it lost: "+
			"fix what the error names and restart, or run vbilling ledger restore. To give that ledger up and start as a new install, move %s away", cfg.DataDir, mirrorDir, err, mirrorDir)
	}
	cfg.MeteringSource, err = sourceid.Load(cfg.DataDir)
	if err != nil {
		log.Fatalf("metering source identity: %v", err)
	}

	sp, err := spool.Open(filepath.Join(cfg.DataDir, "ledger"), spool.Options{Retention: cfg.Retention, ArchiveDir: cfg.ArchiveDir})
	if err != nil {
		log.Fatalf("open ledger: %v", err)
	}
	// Per-request usage records have their own ledger: they are evidence for
	// disputes and input to aggregation, never shipped to a destination.
	recLedgerOpts := spool.Options{Retention: cfg.RecordsRetention}
	if cfg.RecordsArchive {
		// A separate directory: each ledger has its own hash chain.
		recLedgerOpts.ArchiveDir = filepath.Join(cfg.ArchiveDir, "records")
	}
	recSpool, err := spool.Open(filepath.Join(cfg.DataDir, "records"), recLedgerOpts)
	if err != nil {
		log.Fatalf("open records ledger: %v", err)
	}
	dests, err := destinations.NewAll(cfg.Adapters, cfg)
	if err != nil {
		log.Fatalf("initialize adapters: %v", err)
	}
	for _, d := range dests {
		if consumer, ok := d.(interface{ UseLedger(*spool.Spool) }); ok {
			consumer.UseLedger(sp)
		}
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
	sources := health.New(tel)
	// A full ledger volume stops every commit: report it long before. The
	// archive directory is created now, not by the first compaction days
	// later: an archive volume that is not mounted or not writable fails at
	// once, and an empty, healthy one is ok from the first check.
	go diskspace.Watch(ctx, []diskspace.Volume{
		diskspace.LedgerVolume(cfg.DataDir),
		diskspace.ArchiveVolume(cfg.ArchiveDir, spool.EnsureArchiveDir),
	}, sources, tel)
	// The second copy: the archive volume also holds a live mirror of both
	// ledgers and of every other file of DATA_DIR, a few seconds behind. It
	// runs next to metering and never in its way (nil when none is kept;
	// source health then says why).
	mirrorPlan.Report(sources, cfg.ArchiveDir)
	mirror := spool.NewMirror(spool.MirrorOptions{DataDir: cfg.DataDir, Dir: mirrorDir, Ledgers: []*spool.Spool{sp, recSpool},
		Identity: cfg.MeteringSource, Exclude: []string{cfg.ArchiveDir}, Health: sources, Tel: tel})
	go mirror.Run(ctx)
	tenants := pipeline.NewTenantRegistry()
	// Events held until their billing platform counts them (Metronome: a
	// billable metric, unless PRICE_CHECK=off) or prices them
	// (PRICE_CHECK=hold), and events parked until their tenant is set up at a
	// destination. Always opened, so whatever an earlier run held is
	// released.
	held, err := pipeline.OpenHeld(filepath.Join(cfg.DataDir, "held"), false)
	if err != nil {
		log.Fatalf("open held events: %v", err)
	}
	// Held usage is released, unpriced, before its invoice period can be
	// final, after pipeline.MaxUnpricedHold.
	maxHold := min(cfg.PriceCheckMaxHold, pipeline.MaxUnpricedHold)
	var prices *pricing.Service
	if cfg.PriceCheck != pricing.ModeOff {
		prices = pricing.New(sp, dests, tenants, held, sources, tel, pricing.Options{
			Mode: cfg.PriceCheck, Interval: cfg.PriceCheckInterval, Lookback: cfg.PriceCheckLookback, MaxHold: maxHold,
		})
	} else {
		log.Printf("[pricing] %s", pricing.OffNote)
	}
	var gate pipeline.Gate
	if prices != nil {
		gate = prices.Gate()
	}
	disp := pipeline.New(sp, dests, pipeline.Options{
		Tenants:   tenants,
		Bootstrap: func(ctx context.Context, d destinations.Destination) error { return d.Bootstrap(ctx, usage.Catalog()) },
		Held:      held,
		Gate:      gate,
		MaxHold:   maxHold,
		Health:    sources, // paused and returning destinations, tenants a destination cannot set up
	}, tel)
	disc := discovery.NewDiscoverer(kube, dynamicClient, cfg.WatchNamespaces, cfg.PlatformCluster)
	owners := newPlatformResolver(cfg, sources, sp)
	if owners != nil {
		disc.WithBinder(owners)
	}
	pauseKeep, err := pause.ParseKeep(cfg.PauseKeepBilling)
	if err != nil {
		log.Fatalf("%v", err)
	}
	pauses, err := pause.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open pause spans: %v", err)
	}
	coll := metrics.NewCollector(kube, metricsClient, metrics.Options{
		Region:             cfg.Region,
		MeteringSource:     cfg.MeteringSource,
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
		PauseKeep:               pauseKeep,
	})
	coll.UseDynamic(dynamicClient)
	coll.UsePauses(pauses)
	coll.UseHealth(sources)
	// What vBilling first saw of each node of a cluster's own, and the most
	// GPUs it had: a relabel inside the cluster never lowers its bill.
	if err := coll.UsePrivateNodeHistory(filepath.Join(cfg.DataDir, "private-nodes.json")); err != nil {
		log.Fatalf("private node history: %v", err)
	}
	prom, err := coll.NewPromSource(metrics.PromOptions{
		SourceLabel: cfg.PromSourceLabel,
		Lookback:    cfg.PromLookback,
		StatePath:   filepath.Join(cfg.DataDir, "prom-counters.json"),
		Watermark:   func() (time.Time, bool) { return sp.Watermark(metrics.SourcePrometheus) },
		Health:      sources,
	})
	if err != nil {
		log.Fatalf("Prometheus counters: %v", err)
	}
	// Deleted pods and nodes stay billable until their windows are committed.
	retention := cfg.MaxBackfill + 2*cfg.CollectionInterval
	if cfg.WatchDeletions {
		g := metrics.NewGraveyard(retention, time.Now)
		go g.Watch(ctx, kube, metrics.LabelManagedBy, true, true)
		go g.WatchCapacity(ctx, kube, metrics.LabelManagedBy)
		coll.UseGraveyard(g)
		// When each control plane had a ready replica, and when it was
		// deleted: control plane hours follow that, to about a second.
		planes, err := metrics.NewControlPlanes(filepath.Join(cfg.DataDir, "control-planes.json"), retention+time.Hour, time.Now)
		if err != nil {
			log.Fatalf("control plane timeline: %v", err)
		}
		go planes.Watch(ctx, kube, cfg.WatchNamespaces)
		coll.UseControlPlanes(planes)
	}
	var tenantMgr *tenantapi.Manager
	if cfg.TenantAPI {
		ext, err := tenantapi.LoadExternal(cfg.TenantClustersFile)
		if err != nil {
			log.Fatalf("TENANT_CLUSTERS_FILE: %v", err)
		}
		topts := tenantapi.Options{
			SecretName:    cfg.TenantKubeconfigSecret,
			AdminFallback: cfg.TenantAdminFallback,
			External:      ext,
			WatchPods:     cfg.PrivateNodeBilling == "usage",
			Retention:     retention,
		}
		if owners != nil && cfg.PlatformProxy {
			topts.Platform = owners // the only route: kubeconfig Secrets are not read in Platform mode
			log.Printf("Tenant APIs: clusters are reached only through the vCluster Platform proxy at %s", owners.Client().Host())
		}
		mgr := tenantapi.New(ctx, kube, topts)
		coll.UseTenantAPI(mgr)
		tenantMgr = mgr
		disc.WithExternal(mgr.Clusters())
		log.Printf("Cluster APIs: Secret %q per cluster, %d external cluster(s), private nodes billed by %s",
			cfg.TenantKubeconfigSecret, len(ext), cfg.PrivateNodeBilling)
	}
	checks, err := crosscheck.Parse(cfg.CrossChecks)
	if err != nil {
		log.Fatalf("CROSS_CHECKS: %v", err)
	}
	ctrl := controller.New(cfg, disc, coll, sp, tenants, dests, tel)
	ctrl.UsePrometheus(prom)
	ctrl.UsePauses(pauses)
	// Since when each excluded cluster is excluded from metering.
	exclusions, err := pause.OpenSpans(filepath.Join(cfg.DataDir, "exclusion-spans.json"))
	if err != nil {
		log.Fatalf("open exclusion spans: %v", err)
	}
	ctrl.UseExclusions(exclusions)
	ctrl.UseHeld(held) // offboarding waits for usage held until priced
	ctrl.UseHealth(sources)
	ctrl.UseSafeguard(safeguard.New(sp, sources, tel))
	if prices != nil {
		prices.SetReady(ctrl.Ready) // plans are known once tenants are discovered
	}
	if owners != nil {
		ctrl.UseOwners(owners)
	}
	enf := newEnforcer(cfg, kube, ctrl, dests, tel, tenantMgr, pauses, coll, newGateway(cfg, ctrl, formats, tel))
	// A cluster whose API refuses vBilling and a payment that did not lift a
	// hold show as sources, and a cluster that was deleted loses its pause
	// entry.
	enf.UseHealth(sources)
	enf.UseClusters(func() ([]discovery.TenantCluster, bool) { return ctrl.Clusters(), ctrl.Ready() })

	recOpts := records.Options{
		Window:               cfg.CollectionInterval,
		Settle:               cfg.RecordsSettle,
		MaxAge:               min(34*24*time.Hour, cfg.RecordsRetention-time.Hour),
		AllowUnkeyed:         cfg.RecordsAllowUnkeyed,
		RejectUnknownTenants: cfg.RecordsRejectUnknown,
		Region:               cfg.Region,
		MeteringSource:       cfg.MeteringSource,
		BoundTokens:          cfg.IngestTokens, // pinned pulls use the record IDs of the tenant's bound token
	}
	// The aggregator must exist before the API accepts records: it raises the
	// records ledger's clock past every window already committed.
	agg := records.NewAggregator(recSpool, sp, recOpts, sources, tel)
	ingester := records.NewIngester(recSpool, formats, recOpts, ctrl.ResolveTenant, sources, tel)
	// LiteLLM's spend logs, read back as records: they fill what the
	// generic API logger's push lost, under the same record IDs.
	var litellmPuller *records.LiteLLMPuller
	if cfg.RecordsPullLiteLLMURL != "" {
		state, err := kv.Open(filepath.Join(cfg.DataDir, "litellm-pull.json"))
		if err != nil {
			log.Fatalf("open LiteLLM pull state: %v", err)
		}
		litellmPuller = records.NewLiteLLMPuller(ingester, records.LiteLLMPullOptions{
			URL: cfg.RecordsPullLiteLLMURL, Key: cfg.RecordsPullLiteLLMKey,
			Client: cfg.RecordsPullLiteLLMClient, Tenant: cfg.RecordsPullLiteLLMTenant,
			Interval: cfg.RecordsPullLiteLLMInterval, Lookback: cfg.RecordsPullLiteLLMLookback,
			State: state, Ready: ctrl.Ready,
		}, sources, tel)
		log.Printf("Records: pulling LiteLLM spend logs from %s every %s (lookback %s, client %q)",
			cfg.RecordsPullLiteLLMURL, cfg.RecordsPullLiteLLMInterval, cfg.RecordsPullLiteLLMLookback, cfg.RecordsPullLiteLLMClient)
	}
	// Any other gateway's request log, read back the same way (RECORDS_PULL_FILE).
	pullSpecs, err := records.LoadPullSpecs(cfg.RecordsPullFile, formats)
	if err != nil {
		log.Fatalf("RECORDS_PULL_FILE: %v", err)
	}
	var pullers []*records.Puller
	if len(pullSpecs) > 0 {
		state, err := kv.Open(filepath.Join(cfg.DataDir, "records-pull.json"))
		if err != nil {
			log.Fatalf("open records pull state: %v", err)
		}
		for _, spec := range pullSpecs {
			if lb := time.Duration(spec.Lookback); lb > cfg.RecordsRetention-2*time.Hour {
				log.Fatalf("RECORDS_PULL_FILE: pull %s: lookback %s must be at most RECORDS_RETENTION minus 2h (%s)", spec.Name, lb, cfg.RecordsRetention-2*time.Hour)
			}
			if err := ingester.CheckPin(spec.Tenant); err != nil {
				log.Fatalf("RECORDS_PULL_FILE: pull %s: %v", spec.Name, err)
			}
			pullers = append(pullers, records.NewPuller(ingester, spec, state, ctrl.Ready, sources, tel))
			log.Printf("Records: pulling %s (format %s, client %q) every %s, lookback %s", spec.Name, spec.Format, spec.Client,
				time.Duration(spec.Interval), time.Duration(spec.Lookback))
		}
	}
	// Tenants that exist without a cluster: those the records configuration
	// bills to (tenant maps, tenant-bound pulls) and those holding a billing
	// state. No cluster's alias can capture them.
	recordsTenants := recordsConfigTenants(cfg, formats, pullSpecs)
	ctrl.UseKnownTenants(func(id string) bool { return recordsTenants[id] || enf.HasState(id) })
	// vbilling_tenant_billing_state has a series for every tenant vBilling
	// knows, active when it has no hold.
	enf.UseTenants(func() []string { return knownTenantIDs(cfg, tenants, recordsTenants) })

	// The API is never open: without API_TOKEN, a token is generated in
	// DATA_DIR (log its path, never the token). Ingest has a token of its
	// own, never the API token.
	tokenFiles, err := api.PrepareTokens(cfg)
	if err != nil {
		log.Fatalf("API token: %v", err)
	}
	if tokenFiles.API != "" {
		log.Printf("API_TOKEN is not set: the API requires the token vBilling keeps in %s", tokenFiles.API)
	}
	if tokenFiles.Ingest != "" {
		log.Printf("INGEST_TOKEN is not set: gateways send usage with the token vBilling keeps in %s (never the API token)", tokenFiles.Ingest)
	}
	apiServer := api.New(cfg, sp, disp, tenants, ctrl, enf, tel, version).WithHealth(sources).WithPauses(pauses).WithRecords(ingester).WithCrossChecks(checks).WithPricing(prices)
	go apiServer.Run(ctx) // the SKUs tenants have in the ledger and those billing platforms price
	// Read, write and idle timeouts; the raw export streams past them.
	srv := apiServer.HTTPServer(cfg.ListenAddr)

	// Start-up is over: what the Lease guards runs from here on.
	startupOver()
	var wg sync.WaitGroup
	wg.Add(6)
	go func() {
		defer wg.Done()
		agg.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		(&crosscheck.Runner{Spool: sp, Checks: checks, Health: sources, Tel: tel}).Run(ctx)
	}()
	go func() {
		// Forget sources nothing has reported for six hours (deleted
		// clusters, metrics removed from the configuration).
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mirrorPlan.Report(sources, cfg.ArchiveDir) // stated once, so stated again
				sources.Prune(time.Now().Add(-6 * time.Hour))
			}
		}
	}()
	// Converge clusters created or unreachable during a state change, and
	// pause again at once any cluster of a paused tenant that runs. Each
	// loop has its own goroutine: slow cluster APIs or gateway calls in a
	// resync never hold up the guard, and a woken paused cluster is stopped
	// within 15 seconds. A pause cut short at shutdown is finished at the
	// next start.
	go every(ctx, 2*time.Minute, enf.Resync)
	go every(ctx, 15*time.Second, enf.Guard)
	// A wallet refilled without a payment webhook (a commit or credit added
	// to the contract) lifts the pause its threshold notification raised.
	go every(ctx, enforcement.RecheckInterval, enf.RecheckHolds)
	go func() {
		defer wg.Done()
		compactLoop(ctx, recSpool, nil)
	}()
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
	if prices != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prices.Run(ctx)
		}()
	}
	go func() {
		defer wg.Done()
		compactLoop(ctx, sp, cfg.Adapters)
	}()

	if litellmPuller != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			litellmPuller.Run(ctx)
		}()
	}
	for _, p := range pullers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Run(ctx)
		}()
	}

	if err := ctrl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("controller: %v", err)
	}
	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	srv.Shutdown(shutdownCtx)
	wg.Wait()
	mirror.Stop() // one last pass: a clean stop leaves the mirror in step
	if err := sp.Close(); err != nil {
		log.Printf("close ledger: %v", err)
	}
	if err := recSpool.Close(); err != nil {
		log.Printf("close records ledger: %v", err)
	}
	log.Println("vBilling stopped")
}

// newEnforcer sets up billing states. They are always available: Stripe and
// Metronome webhooks set them once their secrets are configured, and
// PUT /api/v1/billing-states/{tenant} sets them from anywhere else, such as
// Lago or a custom billing platform.
// newGateway sets up ENFORCEMENT_LITELLM: suspended and paused tenants'
// LiteLLM teams are blocked. A team is one of the tenant's identities: its
// ID, its aliases, or a value the litellm format's tenantMap maps to it.
func newGateway(cfg *config.Config, ctrl *controller.Controller, formats records.Formats, tel *telemetry.Registry) enforcement.Gateway {
	if !cfg.EnforcementLiteLLM {
		return nil
	}
	if cfg.EnforcementMode != "enforce" {
		log.Printf("ENFORCEMENT_LITELLM is set but ENFORCEMENT_MODE=%s: LiteLLM teams are only blocked in enforce mode", cfg.EnforcementMode)
	}
	state, err := kv.Open(filepath.Join(cfg.DataDir, "gateway-blocks.json"))
	if err != nil {
		log.Fatalf("open gateway state: %v", err)
	}
	tenantMap := formats["litellm"].TenantMap
	// A team is one of a tenant's identities only if its usage bills to that
	// tenant, by the same rules records use: the tenant map, then aliases.
	billsTo := func(team string) string {
		v := team
		if t, ok := tenantMap[team]; ok {
			v = t
		}
		t, _ := ctrl.ResolveTenant(v)
		return t
	}
	identities := func(tenant string) []string {
		var ids []string
		for _, id := range ctrl.GatewayIdentities(tenant) {
			if billsTo(id) == tenant {
				ids = append(ids, id)
			}
		}
		for raw := range tenantMap {
			if billsTo(raw) == tenant {
				ids = append(ids, raw)
			}
		}
		return ids
	}
	log.Printf("Gateway enforcement: LiteLLM teams of suspended and paused tenants are blocked at %s", cfg.RecordsPullLiteLLMURL)
	return gateway.New(state, tel, identities, gateway.NewLiteLLM(cfg.RecordsPullLiteLLMURL, cfg.RecordsPullLiteLLMKey))
}

func newEnforcer(cfg *config.Config, kube kubernetes.Interface, ctrl *controller.Controller, dests []destinations.Destination, tel *telemetry.Registry, tenantMgr *tenantapi.Manager, pauses *pause.Store, coll *metrics.Collector, gw enforcement.Gateway) *enforcement.Enforcer {
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
	log.Printf("Billing states: mode=%s, Stripe webhooks %s, Metronome webhooks %s, paused clusters keep billing %s", cfg.EnforcementMode,
		onOff(cfg.StripeWebhookSecret != ""), onOff(cfg.MetronomeWebhookSecret != ""), cfg.PauseKeepBilling)
	var tenantClient func(ctx context.Context, cl discovery.TenantCluster) (kubernetes.Interface, error)
	if cfg.EnforcementInCluster {
		if tenantMgr == nil {
			log.Fatalf("ENFORCEMENT_IN_CLUSTER needs TENANT_API=true")
		}
		tenantClient = func(ctx context.Context, cl discovery.TenantCluster) (kubernetes.Interface, error) {
			conn, err := tenantMgr.Conn(ctx, cl)
			if errors.Is(err, metrics.ErrNoTenantAPI) {
				return nil, nil // no credentials for this cluster: the control plane policy is all there is
			}
			if err != nil {
				return nil, err
			}
			return conn.Kube, nil
		}
		log.Printf("Enforcement inside clusters: on (needs write access to admission policies in each cluster)")
	}
	opts := enforcement.Options{
		TenantClient:           tenantClient,
		Mode:                   cfg.EnforcementMode,
		StripeWebhookSecret:    cfg.StripeWebhookSecret,
		MetronomeWebhookSecret: cfg.MetronomeWebhookSecret,
		Rules:                  rules,
		Clusters:               ctrl.ClustersOf,
		Resolvers:              resolvers,
		State:                  state,
		Telemetry:              tel,
		IsTenant:               ctrl.KnownTenant,
		Neighbors:              neighbors(cfg, ctrl),
		Pauses:                 pauses,
		OnPause:                ctrl.SetPaused,
		DrainTimeout:           cfg.PauseDrainTimeout,
		KeepNamespaces:         cfg.PauseKeepNamespaces,
		Gateway:                gw,
	}
	if tenantMgr != nil {
		// Metering reads clusters' own APIs, so it bills their own nodes: a
		// pause verifies they are empty before it stops billing them.
		opts.Freeze, opts.OwnNodes = coll.Freeze, coll.OwnNodes
	}
	return enforcement.New(kube, opts)
}

// neighbors lists the clusters in a control plane cluster namespace with
// the tenant each bills to.
func neighbors(cfg *config.Config, ctrl *controller.Controller) func(string) map[string]string {
	src := discovery.TenantSource(cfg.TenantSource)
	return func(ns string) map[string]string {
		out := map[string]string{}
		for _, cl := range ctrl.Clusters() {
			if !cl.External && cl.Namespace == ns {
				out[cl.Name] = cl.TenantID(src)
			}
		}
		return out
	}
}

// recordsConfigTenants returns the tenants the records configuration bills
// to by name: tenant-bound pulls and the targets of formats' tenant maps.
// (Tenants bound to ingest tokens the controller reads from the
// configuration itself.)
func recordsConfigTenants(cfg *config.Config, formats records.Formats, pulls []records.PullSpec) map[string]bool {
	out := map[string]bool{}
	add := func(t string) {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = true
		}
	}
	add(cfg.RecordsPullLiteLLMTenant)
	for _, p := range pulls {
		add(p.Tenant)
	}
	for _, f := range formats {
		for _, t := range f.TenantMap {
			add(t)
		}
	}
	return out
}

// knownTenantIDs lists the tenants vBilling knows without asking for one by
// name: those with a cluster (until they are offboarded), those an ingest
// token is bound to, and those the records configuration bills to.
func knownTenantIDs(cfg *config.Config, tenants *pipeline.TenantRegistry, recordsTenants map[string]bool) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, t := range tenants.All() {
		add(t.ID)
	}
	for _, id := range cfg.IngestTokens {
		add(id)
	}
	for id := range recordsTenants {
		add(id)
	}
	return ids
}

// every runs f every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, f func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f(ctx)
		}
	}
}

// newPlatformResolver sets up TENANT_SOURCE=platform: tenant identity from
// vCluster Platform Tenants, Projects and VirtualClusterInstances.
func newPlatformResolver(cfg *config.Config, sources *health.Registry, sp *spool.Spool) *platform.Resolver {
	if cfg.TenantSource != "platform" {
		return nil
	}
	r, err := platform.New(platform.Options{
		Client:      platformClientOptions(cfg),
		Cluster:     cfg.PlatformCluster,
		Refresh:     cfg.PlatformRefresh,
		OrphanGrace: cfg.PlatformOrphanGrace,
		StatePath:   filepath.Join(cfg.DataDir, "platform-bindings.json"),
		Committed:   func() (time.Time, bool) { return sp.Watermark(spool.SourceCollector) },
		Health:      sources,
		OnBillingIDChange: func(string, string) {
			telemetry.Default.Add("vbilling_tenant_id_changes_total",
				"Times the billing ID of a Tenant or Project changed: the billing platform sees another customer from then on.", nil, 1)
		},
	})
	if err != nil {
		log.Fatalf("vCluster Platform: %v", err)
	}
	if cfg.PlatformInsecure {
		log.Printf("WARNING: PLATFORM_INSECURE=true: the Platform's TLS certificate is not verified; set PLATFORM_CA_FILE instead")
	}
	if cfg.PlatformCluster == "" {
		log.Printf("WARNING: PLATFORM_CLUSTER is not set: instances on every connected cluster can match this control plane cluster's workloads")
	}
	log.Printf("Tenants: vCluster Platform at %s (Tenant, else Project), refreshed every %s", cfg.PlatformURL, cfg.PlatformRefresh)
	return r
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

// getKubeConfig returns the Kubernetes config and the namespace vBilling
// runs in: the pod's own, or the kubeconfig context's ("" when it names none).
func getKubeConfig() (*rest.Config, string, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		log.Println("Using in-cluster Kubernetes config")
		ns, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		return cfg, strings.TrimSpace(string(ns)), nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	ns := ""
	if raw, err := cc.RawConfig(); err == nil {
		if c := raw.Contexts[raw.CurrentContext]; c != nil {
			ns = c.Namespace
		}
	}
	log.Printf("Using kubeconfig (%s)", os.Getenv("KUBECONFIG"))
	return cfg, ns, nil
}

// holdLease takes the vbilling Lease in namespace (see internal/lease) and
// stops the process if it is ever lost. A second instance waits here, alive
// but not Ready. Without a namespace, or without RBAC for Leases, vBilling
// runs without the guard and says so.
func holdLease(ctx context.Context, kube kubernetes.Interface, namespace, listen string) *lease.Held {
	if namespace == "" {
		log.Printf("WARNING: vBilling does not know its namespace (outside a cluster, set one on the kubeconfig context), so it cannot take its Lease and runs without the single-instance guard: never run two vBilling instances against the same clusters")
		return nil
	}
	held, err := lease.Acquire(ctx, kube, lease.Options{Namespace: namespace, Listen: listen})
	switch {
	case err == nil:
	case errors.Is(err, lease.ErrForbidden):
		log.Printf("WARNING: %v: running without the single-instance guard. Grant get, create and update on leases in namespace %s (the chart's Role does)", err, namespace)
		return nil
	case ctx.Err() != nil:
		log.Println("vBilling stopped while waiting for its Lease")
		os.Exit(0)
	case err != nil:
		log.Fatalf("Lease %s/%s: %v", namespace, lease.Name, err)
	}
	go func() {
		<-held.Lost()
		log.Fatalf("[lease] lost Lease %s/%s: stopping now so another instance can take over; the ledger and backfill resume where this one stopped", namespace, lease.Name)
	}()
	return held
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
