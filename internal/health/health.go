// Package health tracks whether vBilling's usage sources are working.
//
// A billing pipeline that fails quietly is worse than one that stops: a
// Prometheus query that returns nothing, a tenant API that refuses the
// credentials or a gateway that stopped posting all look like "no usage".
// Every source reports here after each attempt, so the status API, the
// dashboard and /metrics can show the last success, the last error and a
// hint for each one, and the log records every change of state instead of
// only the first failure.
package health

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// Status of a source.
type Status string

const (
	// OK means the last attempt returned data.
	OK Status = "ok"
	// Empty means the source answered but returned nothing. For a billed
	// metric this is usually a selector that matches no series.
	Empty Status = "empty"
	// Failing means the last attempt errored.
	Failing Status = "failing"
	// Diverged is used by cross-checks whose sources disagree.
	Diverged Status = "diverged"
)

// Source is the health of one usage source.
type Source struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`              // prometheus, tenant-api, metrics-server, records, crosscheck
	Subject string `json:"subject,omitempty"` // tenant cluster, client or tenant the source is about
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
	// Series is how many series, records or rows the last success returned.
	Series              int       `json:"series,omitempty"`
	Since               time.Time `json:"since"` // when the current status began
	LastAttempt         time.Time `json:"last_attempt"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastError           time.Time `json:"last_error,omitempty"`
	LastErrorMessage    string    `json:"last_error_message,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
	ConsecutiveEmpty    int       `json:"consecutive_empty,omitempty"`
}

// Registry is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	sources map[string]*Source
	tel     *telemetry.Registry
	now     func() time.Time
	logf    func(format string, args ...any)
}

// New returns an empty registry that also exports gauges to tel.
func New(tel *telemetry.Registry) *Registry {
	if tel == nil {
		tel = telemetry.New()
	}
	return &Registry{sources: map[string]*Source{}, tel: tel, now: time.Now, logf: log.Printf}
}

// SetClock replaces the clock (tests).
func (r *Registry) SetClock(now func() time.Time) { r.now = now }

// SetLogger replaces the logger (tests).
func (r *Registry) SetLogger(logf func(string, ...any)) { r.logf = logf }

// OK records a successful attempt that returned n series or records.
func (r *Registry) OK(name, kind, subject string, n int) {
	r.report(name, kind, subject, OK, "", "", n)
}

// Fail records a failed attempt.
func (r *Registry) Fail(name, kind, subject string, err error, hint string) {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	r.report(name, kind, subject, Failing, msg, hint, 0)
}

// Empty records an attempt that succeeded but returned nothing.
func (r *Registry) Empty(name, kind, subject, message, hint string) {
	r.report(name, kind, subject, Empty, message, hint, 0)
}

// Set records an arbitrary status (cross-checks).
func (r *Registry) Set(name, kind, subject string, status Status, message, hint string) {
	r.report(name, kind, subject, status, message, hint, 0)
}

func (r *Registry) report(name, kind, subject string, status Status, message, hint string, n int) {
	now := r.now().UTC()
	r.mu.Lock()
	s, ok := r.sources[name]
	if !ok {
		s = &Source{Name: name, Kind: kind, Subject: subject, Status: status, Since: now}
		r.sources[name] = s
	}
	prev := s.Status
	changed := !ok || prev != status
	if changed {
		s.Since = now
	}
	s.Status, s.LastAttempt = status, now
	s.Message, s.Hint = message, hint
	switch status {
	case OK:
		s.LastSuccess, s.Series = now, n
		s.ConsecutiveFailures, s.ConsecutiveEmpty = 0, 0
	case Failing:
		s.LastError, s.LastErrorMessage = now, message
		s.ConsecutiveFailures++
		s.ConsecutiveEmpty = 0
	case Empty:
		s.LastSuccess = now // the source answered
		s.ConsecutiveEmpty++
		s.ConsecutiveFailures = 0
	}
	failures, empties := s.ConsecutiveFailures, s.ConsecutiveEmpty
	r.mu.Unlock()

	// Log every change of state, and repeated problems at 1, 10, 100, ...
	// so a source that stays broken is visible without flooding the log.
	switch {
	case changed && status == OK && ok:
		r.logf("[health] %s recovered (was %s)", name, prev)
	case status == Failing && (changed || isPow10(failures)):
		r.logf("[health] %s failing (%d in a row): %s%s", name, failures, message, hintSuffix(hint))
	case status == Empty && (changed || isPow10(empties)):
		r.logf("[health] %s returned no data (%d in a row): %s%s", name, empties, message, hintSuffix(hint))
	case status == Diverged && changed:
		r.logf("[health] %s: %s%s", name, message, hintSuffix(hint))
	}
	r.export()
}

func hintSuffix(h string) string {
	if h == "" {
		return ""
	}
	return " (hint: " + h + ")"
}

func isPow10(n int) bool {
	for n >= 10 && n%10 == 0 {
		n /= 10
	}
	return n == 1
}

// export publishes per-kind counts by status. Per-source series would
// explode label cardinality (metrics x clusters), so details stay in the API.
func (r *Registry) export() {
	r.mu.Lock()
	counts := map[[2]string]int{}
	kinds := map[string]bool{}
	for _, s := range r.sources {
		counts[[2]string{s.Kind, string(s.Status)}]++
		kinds[s.Kind] = true
	}
	r.mu.Unlock()
	for k := range kinds {
		for _, st := range []Status{OK, Empty, Failing, Diverged} {
			r.tel.Set("vbilling_sources", "Usage sources by kind and status (see GET /api/v1/sources).",
				map[string]string{"kind": k, "status": string(st)}, float64(counts[[2]string{k, string(st)}]))
		}
	}
}

// List returns every source, problems first, then by name.
func (r *Registry) List() []Source {
	r.mu.Lock()
	out := make([]Source, 0, len(r.sources))
	for _, s := range r.sources {
		out = append(out, *s)
	}
	r.mu.Unlock()
	rank := map[Status]int{Failing: 0, Diverged: 1, Empty: 2, OK: 3}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Problems returns the sources that are not OK.
func (r *Registry) Problems() []Source {
	var out []Source
	for _, s := range r.List() {
		if s.Status != OK {
			out = append(out, s)
		}
	}
	return out
}

// Summary counts sources by status.
func (r *Registry) Summary() map[Status]int {
	out := map[Status]int{}
	for _, s := range r.List() {
		out[s.Status]++
	}
	return out
}

// Prune forgets sources not reported since cutoff (deleted tenant
// clusters, metrics removed from the configuration).
func (r *Registry) Prune(cutoff time.Time) {
	r.mu.Lock()
	for k, s := range r.sources {
		if s.LastAttempt.Before(cutoff) {
			delete(r.sources, k)
		}
	}
	r.mu.Unlock()
	r.export()
}

// Name builds a source name from parts, e.g. Name("prometheus", metric, cluster).
func Name(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "/")
}

// Describe renders a source for logs and errors.
func (s Source) Describe() string {
	return fmt.Sprintf("%s: %s %s", s.Name, s.Status, s.Message)
}
