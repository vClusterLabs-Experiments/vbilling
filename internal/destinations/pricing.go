package destinations

import (
	"context"
	"sort"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Price coverage statuses. A billing platform that receives usage it has no
// price for rates it at $0 without an error ("rate miss"), so vBilling asks
// each billing platform, read-only, whether it will price what vBilling sends.
const (
	// PriceCovered: the platform has a non-zero price for the series.
	PriceCovered = "priced"
	// PriceMissing: no price applies; the platform would bill the usage at $0.
	PriceMissing = "unpriced"
	// PriceZero: a price exists and is 0. That can be intentional (a free
	// tier, an internal tenant), so it is information, not a gap.
	PriceZero = "zero"
	// PriceUnknown: the check cannot tell (for example no rate card is
	// configured, so vBilling does not know which one prices the usage).
	PriceUnknown = "unknown"
)

// PriceSeries is one usage series as a billing platform prices it.
type PriceSeries struct {
	// Plan is the plan, rate card or price tag the usage is rated on ("" when
	// the platform has no such notion for vBilling's usage).
	Plan   string `json:"plan,omitempty"`
	Metric string `json:"metric"`
	SKU    string `json:"sku,omitempty"`
	// Dims are the other dimensions the platform can price on (region,
	// capacity type, ...), from PriceChecker.PriceDims. Missing values are "".
	Dims map[string]string `json:"dims,omitempty"`
	// Catalog marks a billable metric with no usage seen yet: the question is
	// whether the platform prices the metric at all.
	Catalog bool `json:"catalog,omitempty"`
}

// Key identifies a series within one destination.
func (s PriceSeries) Key() string {
	var b strings.Builder
	b.WriteString(s.Plan)
	b.WriteByte(0)
	b.WriteString(s.Metric)
	b.WriteByte(0)
	if s.Catalog {
		b.WriteString("\x01catalog")
	} else {
		b.WriteString(s.SKU)
	}
	keys := make([]string, 0, len(s.Dims))
	for k := range s.Dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s.Dims[k])
	}
	return b.String()
}

// PriceCoverage is a billing platform's answer for one series.
type PriceCoverage struct {
	Series PriceSeries `json:"series"`
	Status string      `json:"status"`
	// Detail says what the check found (the product and rate, the Stripe
	// price, the Lago charge or filter) or what is missing.
	Detail string `json:"detail,omitempty"`
	// Hint names what Finance has to create when Status is unpriced.
	Hint string `json:"hint,omitempty"`
}

// PriceChecker is implemented by billing adapters that can tell whether the
// billing platform prices the usage vBilling sends it. Checks only read
// pricing: vBilling never writes prices or rates.
type PriceChecker interface {
	// PricePlan returns the plan or rate card t's usage is rated on at this
	// platform, "" when it has none.
	PricePlan(t usage.Tenant) string
	// PriceDims lists the dimensions besides the SKU that the platform can
	// price metric on. Each combination of their values is its own series.
	PriceDims(metric string) []string
	// CheckPrices reports coverage for each series, in any order. An error
	// means the platform could not be read; nothing is concluded from it.
	CheckPrices(ctx context.Context, series []PriceSeries) ([]PriceCoverage, error)
}

// SeriesPreparer is implemented by adapters that create what a new series
// needs before anyone can price it (a Stripe meter, a Lago billable metric
// filter value). It never creates a price. vBilling calls it when a series
// first appears, so a held series can be priced before its first delivery.
type SeriesPreparer interface {
	PrepareSeries(ctx context.Context, series []PriceSeries) error
}

// TenantRefresher is implemented by adapters whose tenants must be ensured
// again once a series becomes priced (Stripe with STRIPE_AUTO_SUBSCRIBE adds
// new prices to existing subscriptions in EnsureTenant).
type TenantRefresher interface {
	RefreshTenantsOnPriceChange() bool
}

// FieldValue returns an event's value for a pricing key: sku, region and
// project are top-level fields, anything else is a dimension.
func FieldValue(e *usage.Event, key string) string {
	switch key {
	case "sku":
		return e.SKU
	case "region":
		return e.Region
	case "project":
		return e.Project
	default:
		return e.Dim(key)
	}
}

// SeriesOf returns the series an event belongs to at a price checker.
func SeriesOf(pc PriceChecker, plan string, e *usage.Event) PriceSeries {
	s := PriceSeries{Plan: plan, Metric: e.Metric, SKU: e.SKU}
	for _, k := range pc.PriceDims(e.Metric) {
		if k == "sku" {
			continue
		}
		if s.Dims == nil {
			s.Dims = map[string]string{}
		}
		s.Dims[k] = FieldValue(e, k)
	}
	return s
}

// EventFor builds a minimal event carrying a series' values, for adapters
// that derive names from events (Stripe meter names).
func (s PriceSeries) EventFor() usage.Event {
	e := usage.Event{Metric: s.Metric, SKU: s.SKU}
	for k, v := range s.Dims {
		switch k {
		case "region":
			e.Region = v
		case "project":
			e.Project = v
		default:
			if e.Dimensions == nil {
				e.Dimensions = map[string]string{}
			}
			e.Dimensions[k] = v
		}
	}
	return e
}

// DescribeSeries renders a series for messages: "vcluster_gpu_hours
// sku=NVIDIA-H100 region=us-east (rate card ai-cloud)".
func DescribeSeries(s PriceSeries) string {
	var b strings.Builder
	b.WriteString(s.Metric)
	if s.Catalog {
		b.WriteString(" (no usage yet)")
	} else if s.SKU != "" {
		b.WriteString(" sku=" + s.SKU)
	}
	keys := make([]string, 0, len(s.Dims))
	for k := range s.Dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s.Dims[k] != "" {
			b.WriteString(" " + k + "=" + s.Dims[k])
		}
	}
	return b.String()
}
