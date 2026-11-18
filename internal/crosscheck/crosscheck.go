// Package crosscheck compares two independent measurements of the same
// usage, such as the tokens an AI gateway reports per request and the
// tokens the inference engine's own counters show.
//
// Billing reads one source; the other is recorded as an informational
// metric next to it in the same ledger. Every hour, each tenant's totals are
// compared and a divergence above the tolerance is reported in source
// health, /metrics and GET /api/v1/crosscheck. Disagreement does not stop
// billing: it tells you which tenant, which hour and by how much, so a
// dropped gateway batch or a lost scrape is caught before an invoice goes
// out instead of in a dispute afterwards.
package crosscheck

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Check compares Primary (billed) with Secondary (a second source).
type Check struct {
	Primary   string  `json:"primary"`
	Secondary string  `json:"secondary"`
	Tolerance float64 `json:"tolerance"` // relative, e.g. 0.02 for 2%
}

// Name identifies the check.
func (c Check) Name() string { return c.Primary + "~" + c.Secondary }

// Parse reads CROSS_CHECKS: "primary=secondary[:tolerance%],...", e.g.
// "inference_output_tokens=vllm_output_tokens:2%". The default tolerance
// is 1%.
func Parse(spec string) ([]Check, error) {
	var out []Check
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pair, tol, hasTol := strings.Cut(item, ":")
		p, s, ok := strings.Cut(pair, "=")
		if !ok || p == "" || s == "" {
			return nil, fmt.Errorf("cross-check %q: expected primary=secondary[:tolerance%%]", item)
		}
		c := Check{Primary: strings.TrimSpace(p), Secondary: strings.TrimSpace(s), Tolerance: 0.01}
		if hasTol {
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(tol), "%"), 64)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("cross-check %q: tolerance must be a percentage", item)
			}
			c.Tolerance = v / 100
		}
		for _, m := range []string{c.Primary, c.Secondary} {
			if _, ok := usage.Lookup(m); !ok {
				return nil, fmt.Errorf("cross-check %q: unknown metric %q", item, m)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// Row is one tenant and bucket of one check.
type Row struct {
	Check     string    `json:"check"`
	Tenant    string    `json:"tenant"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Primary   float64   `json:"primary"`
	Secondary float64   `json:"secondary"`
	// Divergence is |primary - secondary| / max(primary, secondary).
	Divergence float64 `json:"divergence"`
	OK         bool    `json:"ok"`
}

// Evaluate compares the checks over [from, to) in buckets of the given size.
func Evaluate(sp *spool.Spool, checks []Check, from, to time.Time, bucket time.Duration) ([]Row, error) {
	if len(checks) == 0 {
		return nil, nil
	}
	type key struct {
		check, tenant string
		start         time.Time
	}
	sums := map[key]*[2]float64{}
	err := sp.Scan(from, to, func(e *usage.Event) error {
		if e.WindowStart.Before(from) || !e.WindowStart.Before(to) {
			return nil
		}
		b := e.WindowStart.Truncate(bucket)
		for _, c := range checks {
			side := -1
			switch e.Metric {
			case c.Primary:
				side = 0
			case c.Secondary:
				side = 1
			}
			if side < 0 {
				continue
			}
			k := key{c.Name(), e.Tenant, b}
			if sums[k] == nil {
				sums[k] = &[2]float64{}
			}
			sums[k][side] += e.Quantity
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	tol := map[string]float64{}
	for _, c := range checks {
		tol[c.Name()] = c.Tolerance
	}
	out := make([]Row, 0, len(sums))
	for k, v := range sums {
		r := Row{Check: k.check, Tenant: k.tenant, From: k.start, To: k.start.Add(bucket),
			Primary: usage.Round(v[0]), Secondary: usage.Round(v[1])}
		if hi := math.Max(r.Primary, r.Secondary); hi > 0 {
			r.Divergence = math.Abs(r.Primary-r.Secondary) / hi
		}
		r.OK = r.Divergence <= tol[k.check]
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].From.Equal(out[j].From) {
			return out[i].From.Before(out[j].From)
		}
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		return out[i].Tenant < out[j].Tenant
	})
	return out, nil
}

// Runner evaluates the last complete hour periodically.
type Runner struct {
	Spool  *spool.Spool
	Checks []Check
	// Delay waits for both sources to settle before an hour is checked
	// (records settle, Prometheus backfill). Default 10m.
	Delay  time.Duration
	Health *health.Registry
	Tel    *telemetry.Registry
	Now    func() time.Time
}

// Run checks every five minutes until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	if len(r.Checks) == 0 {
		return
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		r.Once()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once evaluates the last complete hour and reports it.
func (r *Runner) Once() []Row {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	delay := r.Delay
	if delay <= 0 {
		delay = 10 * time.Minute
	}
	end := now().UTC().Add(-delay).Truncate(time.Hour)
	start := end.Add(-time.Hour)
	rows, err := Evaluate(r.Spool, r.Checks, start, end, time.Hour)
	if err != nil {
		if r.Health != nil {
			r.Health.Fail("crosscheck", "crosscheck", "", err, "")
		}
		return nil
	}
	for _, row := range rows {
		name := health.Name("crosscheck", row.Check, row.Tenant)
		msg := fmt.Sprintf("%s-%s: %s = %v, %s = %v (%.2f%% apart)", row.From.Format("2006-01-02 15:04"), row.To.Format("15:04"),
			strings.Split(row.Check, "~")[0], row.Primary, strings.Split(row.Check, "~")[1], row.Secondary, 100*row.Divergence)
		if r.Health != nil {
			if row.OK {
				r.Health.OK(name, "crosscheck", row.Tenant, 1)
			} else {
				r.Health.Set(name, "crosscheck", row.Tenant, health.Diverged, msg,
					"the two sources disagree: look for dropped gateway batches (records rejected or never sent), lost scrapes, or requests that reached one source only")
			}
		}
		if r.Tel != nil {
			r.Tel.Set("vbilling_crosscheck_divergence_ratio", "Relative difference between two sources of the same usage in the last checked hour.",
				map[string]string{"check": row.Check, "tenant": row.Tenant}, row.Divergence)
		}
	}
	return rows
}
