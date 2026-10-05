// Package telemetry is a dependency-free Prometheus text exposition registry
// for vBilling's own health: delivery lag, failures, dead letters, windows.
package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

type kind int

const (
	counter kind = iota
	gauge
)

type family struct {
	help   string
	kind   kind
	series map[string]float64 // rendered label set -> value
}

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// Default is the process-wide registry.
var Default = New()

// New returns an empty registry.
func New() *Registry { return &Registry{families: map[string]*family{}} }

func (r *Registry) fam(name, help string, k kind) *family {
	f, ok := r.families[name]
	if !ok {
		f = &family{help: help, kind: k, series: map[string]float64{}}
		r.families[name] = f
	}
	return f
}

// Add increments a counter.
func (r *Registry) Add(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fam(name, help, counter).series[renderLabels(labels)] += v
}

// Set sets a gauge.
func (r *Registry) Set(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fam(name, help, gauge).series[renderLabels(labels)] = v
}

// Get returns a series value (tests and status endpoints).
func (r *Registry) Get(name string, labels map[string]string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		return f.series[renderLabels(labels)]
	}
	return 0
}

// WriteText writes the Prometheus text exposition format.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := r.families[n]
		typ := "counter"
		if f.kind == gauge {
			typ = "gauge"
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, f.help, n, typ); err != nil {
			return err
		}
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "%s%s %s\n", n, k, formatValue(f.series[k])); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatValue(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return fmt.Sprintf("%g", v)
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(labels[k])
		fmt.Fprintf(&b, `%s="%s"`, k, v)
	}
	b.WriteByte('}')
	return b.String()
}
