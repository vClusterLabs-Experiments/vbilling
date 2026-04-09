package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Lago connection
	LagoAPIURL string
	LagoAPIKey string

	// Collection intervals
	CollectionInterval time.Duration // how often to scrape metrics and send events
	ReconcileInterval  time.Duration // how often to discover new/removed vClusters

	// Billing plan
	DefaultPlanCode string
	BillingCurrency string

	// Optional: Prometheus for DCGM + network metrics
	PrometheusURL string

	// Node cost attribution
	SpotDiscountPercent float64 // e.g. 60 means spot nodes cost 40% of on-demand

	// Namespace filter (empty = all namespaces)
	WatchNamespaces []string
}

func Load() *Config {
	c := &Config{
		LagoAPIURL:         envOr("LAGO_API_URL", "http://localhost:3000"),
		LagoAPIKey:         envOr("LAGO_API_KEY", ""),
		CollectionInterval: envDuration("COLLECTION_INTERVAL", 60*time.Second),
		ReconcileInterval:  envDuration("RECONCILE_INTERVAL", 30*time.Second),
		DefaultPlanCode:    envOr("DEFAULT_PLAN_CODE", "vcluster-standard"),
		BillingCurrency:    envOr("BILLING_CURRENCY", "USD"),
		PrometheusURL:      envOr("PROMETHEUS_URL", ""),
		SpotDiscountPercent: envFloat("SPOT_DISCOUNT_PERCENT", 60),
	}

	// Parse watch namespaces from comma-separated env var
	if ns := os.Getenv("WATCH_NAMESPACES"); ns != "" {
		for _, n := range strings.Split(ns, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				c.WatchNamespaces = append(c.WatchNamespaces, n)
			}
		}
	}

	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return fallback
}
