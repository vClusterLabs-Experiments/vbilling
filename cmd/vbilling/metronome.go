package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

const metronomeUsage = `Usage: vbilling metronome <command>

Helpers for a Metronome account that is managed as configuration. Nothing
here calls Metronome.

Commands:
  billable-metrics   print the billable metrics vBilling's usage needs, as
                     JSON bodies for POST /v1/billable-metrics/create

Run "vbilling metronome <command> -h" for the flags of a command.
`

// runMetronomeCmd runs "vbilling metronome ..." and returns the exit code.
func runMetronomeCmd(args []string) int {
	return metronomeMain(args, os.Stdout, os.Stderr)
}

func metronomeMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, metronomeUsage)
		return 2
	}
	switch args[0] {
	case "billable-metrics":
		return metronomeBillableMetrics(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, metronomeUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown metronome command %q\n\n%s", args[0], metronomeUsage)
		return 2
	}
}

func metronomeBillableMetrics(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vbilling metronome billable-metrics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: vbilling metronome billable-metrics

Prints a JSON array with one billable metric per metric vBilling bills: the
SUM of the value property, filtered on the event type (the metric code),
requiring value and every group-key property, with one compound group key.
Each entry is a body for Metronome's POST /v1/billable-metrics/create, to add
to the configuration Finance applies to Metronome. With
METRONOME_PROVISION=true vBilling creates the same billable metrics itself.

Custom metrics are included when the settings that declare them are set
(CUSTOM_METRICS, CUSTOM_METRICS_FILE, PROMETHEUS_METRICS_FILE,
RECORDS_MAPPINGS_FILE, RECORDS_LITELLM), as they are in vBilling's pod. With
ALLOWANCES_FILE, storage is billed as vcluster_storage_overage_gb_hours: the
list holds that metric, and not vcluster_storage_gb_hours.
`)
	}
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if err := declareCatalog(config.Load()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var out []metronome.BillableMetric
	for _, def := range usage.Billable() {
		out = append(out, metronome.BillableMetricFor(def))
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// declareCatalog registers the custom metrics vBilling declares from the
// same settings, in the same order as main.
func declareCatalog(cfg *config.Config) error {
	customs, err := usage.ParseCustomMetrics(cfg.CustomMetrics)
	if err != nil {
		return fmt.Errorf("CUSTOM_METRICS: %w", err)
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
	}
	for _, d := range customs {
		if err := usage.RegisterCustom(d); err != nil {
			return fmt.Errorf("CUSTOM_METRICS: %w", err)
		}
	}
	formats, err := records.LoadFormats(cfg.RecordsMappingsFile, cfg.RecordsLiteLLM)
	if err != nil {
		return fmt.Errorf("RECORDS_MAPPINGS_FILE: %w", err)
	}
	if err := formats.Declare(); err != nil {
		return fmt.Errorf("records formats: %w", err)
	}
	if _, err := metrics.LoadPromMetrics(cfg.PrometheusMetrics); err != nil {
		return fmt.Errorf("PROMETHEUS_METRICS_FILE: %w", err)
	}
	allowances, err := metrics.LoadAllowances(cfg.AllowancesFile)
	if err != nil {
		return fmt.Errorf("ALLOWANCES_FILE: %w", err)
	}
	usage.UseStorageAllowances(len(allowances) > 0)
	return nil
}
