package metrics

import (
	"fmt"
	"maps"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Storage allowances (ALLOWANCES_FILE): storage included in a price, such as
// 32 GiB with every node.
//
// A rating engine applies included units to a customer's total for a
// product. It cannot apply them per node, per cluster or per volume: two
// nodes with a 20 GiB volume each, and 32 GiB included per node, include
// nothing more than one 32 GiB tier, and 8 GiB of the 40 are billed. vBilling
// applies each allowance where it is defined, from the same lifetimes
// vcluster_storage_gb_hours is metered from: the second a volume was bound,
// each capacity it had, the second it was deleted, and every second a node
// was billed. What is above the allowance, at each instant of a window, is
// billed as vcluster_storage_overage_gb_hours.

// Allowance is one entry of ALLOWANCES_FILE.
type Allowance struct {
	// Metric is the metric the allowance applies to. Only
	// vcluster_storage_gb_hours takes one.
	Metric string `json:"metric"`
	// Included is how many GiB are included.
	Included float64 `json:"included"`
	// Per says what includes them: each of the cluster's own nodes while it
	// is billed (node), the cluster (cluster), or each volume (volume).
	Per string `json:"per"`
	// StorageClasses share one allowance, in the order listed: the first
	// class listed takes from it first. Classes no entry lists have none.
	// Without StorageClasses, every class no other entry lists has an
	// allowance of its own.
	StorageClasses []string `json:"storageClasses,omitempty"`
}

// What includes an allowance (Allowance.Per).
const (
	PerNode    = "node"
	PerCluster = "cluster"
	PerVolume  = "volume"
)

// LoadAllowances reads ALLOWANCES_FILE; an empty path is no allowances.
func LoadAllowances(path string) ([]Allowance, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseAllowances(data, path)
}

// ParseAllowances parses and checks a YAML list of allowances. name says
// where it comes from, in errors.
func ParseAllowances(data []byte, name string) ([]Allowance, error) {
	var list []Allowance
	if err := yaml.UnmarshalStrict(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	listedBy := map[string]int{}
	everyClass := 0 // the entry without storageClasses, 1-based
	for i, a := range list {
		n := i + 1
		switch {
		case a.Metric == "":
			return nil, fmt.Errorf("%s: allowance %d: metric is required: %s", name, n, usage.MetricStorageGBHours)
		case a.Metric != usage.MetricStorageGBHours:
			return nil, fmt.Errorf("%s: allowance %d: metric %q takes no allowance: only %s does", name, n, a.Metric, usage.MetricStorageGBHours)
		case math.IsNaN(a.Included) || math.IsInf(a.Included, 0) || a.Included <= 0:
			return nil, fmt.Errorf("%s: allowance %d: included must be a number of GiB above 0, not %v", name, n, a.Included)
		}
		switch a.Per {
		case PerNode, PerCluster, PerVolume:
		case "":
			return nil, fmt.Errorf("%s: allowance %d: per is required: node, cluster or volume", name, n)
		default:
			return nil, fmt.Errorf("%s: allowance %d: per %q must be node, cluster or volume", name, n, a.Per)
		}
		if a.StorageClasses == nil {
			if everyClass > 0 {
				return nil, fmt.Errorf("%s: allowances %d and %d both apply to every storage class: list the classes of all but one in storageClasses", name, everyClass, n)
			}
			everyClass = n
			continue
		}
		if len(a.StorageClasses) == 0 {
			return nil, fmt.Errorf("%s: allowance %d: storageClasses is empty: list the classes, or leave it out to apply the allowance to every class", name, n)
		}
		seen := map[string]bool{}
		for _, class := range a.StorageClasses {
			switch {
			case strings.TrimSpace(class) == "":
				return nil, fmt.Errorf("%s: allowance %d: a storage class in storageClasses is empty (a claim without one is metered under the class default)", name, n)
			case seen[class]:
				return nil, fmt.Errorf("%s: allowance %d: storageClasses lists %q twice", name, n, class)
			case listedBy[class] > 0:
				return nil, fmt.Errorf("%s: allowance %d: storage class %q is in allowance %d too: a class has one allowance", name, n, class, listedBy[class])
			}
			seen[class] = true
			listedBy[class] = n
		}
	}
	return list, nil
}

// allowanceRules are the allowances by storage class.
type allowanceRules struct {
	listed map[string]*Allowance // a class -> the entry that lists it
	every  *Allowance            // the entry without storageClasses, if any
}

func newAllowanceRules(list []Allowance) *allowanceRules {
	if len(list) == 0 {
		return nil
	}
	r := &allowanceRules{listed: map[string]*Allowance{}}
	for i := range list {
		a := list[i]
		a.StorageClasses = append([]string(nil), a.StorageClasses...)
		if a.StorageClasses == nil {
			r.every = &a
			continue
		}
		for _, class := range a.StorageClasses {
			r.listed[class] = &a
		}
	}
	return r
}

// of returns the allowance of a storage class, nil for none.
func (r *allowanceRules) of(class string) *Allowance {
	if a, ok := r.listed[class]; ok {
		return a
	}
	return r.every
}

// properties are what an event records of the allowance applied to it.
func (a *Allowance) properties() map[string]any {
	out := map[string]any{"included_gib": a.Included, "allowance_per": a.Per}
	if len(a.StorageClasses) > 0 {
		out["allowance_storage_classes"] = strings.Join(a.StorageClasses, ",")
	}
	return out
}

// storageUses is what one side of a cluster's metering (the control plane
// cluster, or the cluster's own API) billed of the cluster's volumes and of
// its own nodes, over time: what its allowances apply to. A nil
// *storageUses records nothing.
type storageUses struct {
	// coverable says which volumes an allowance may cover (nil: all). The
	// control plane's own volumes, billed with METER_CONTROL_PLANE, are not
	// the tenant's storage: no allowance covers them.
	coverable func(labels map[string]string) bool
	volumes   []volumeUse
	nodes     [][2]time.Time // the times each of the cluster's own nodes was billed; a zero end is open
}

// volumeUse is one volume: the times it was billed, at the capacity it had.
type volumeUse struct {
	class string
	parts []volumePart
}

type volumePart struct {
	from, to time.Time // a zero to: still billed
	gib      float64
}

// keeps reports whether a volume with these labels is recorded.
func (u *storageUses) keeps(labels map[string]string) bool {
	return u != nil && (u.coverable == nil || u.coverable(labels))
}

// volume records a volume: the times it was billed, at the capacity it had.
func (u *storageUses) volume(class string, parts []volumePart) {
	u.volumes = append(u.volumes, volumeUse{class: class, parts: parts})
}

// node records the times a node of the cluster's own was billed.
func (u *storageUses) node(billed [][2]time.Time) {
	if u != nil {
		u.nodes = append(u.nodes, billed...)
	}
}

// coverStorage records, for every window, how much of the cluster's storage
// events its allowances cover (accumulator.cover). It runs once per side of
// the cluster's metering, after its volumes and nodes are metered: a
// cluster's own volumes and nodes are on one side, the control plane
// cluster for clusters on shared or dedicated nodes, the cluster's own API
// for clusters with their own nodes. base builds the cluster's events.
func (c *Collector) coverStorage(uses *storageUses, windows []Window, accs map[time.Time]*accumulator, base baseFn) {
	if c.allowances == nil || uses == nil || len(uses.volumes) == 0 {
		return
	}
	for _, w := range windows {
		covered := c.allowances.covered(uses, w)
		for _, class := range classesOf(covered) {
			if q := covered[class]; q > 0 {
				tmpl := base(usage.MetricStorageGBHours, nil)
				tmpl.SKU = class
				accs[w.End].cover(tmpl, q)
			}
		}
	}
}

// covered returns, per storage class, the GiB-hours of the window the
// allowances cover.
func (r *allowanceRules) covered(uses *storageUses, w Window) map[string]float64 {
	// The parts of each class in the window, and which allowance each
	// class takes from.
	parts := map[string][]volumePart{}
	for _, v := range uses.volumes {
		if r.of(v.class) == nil {
			continue
		}
		for _, p := range v.parts {
			if usage.Overlap(p.from, p.to, w.Start, w.End) > 0 {
				parts[v.class] = append(parts[v.class], p)
			}
		}
	}
	out := map[string]float64{}
	pooled := map[*Allowance][]string{} // node and cluster allowances: their classes, in the order they take from it
	for _, class := range classesOf(parts) {
		a := r.of(class)
		switch {
		case a.Per == PerVolume:
			// Each volume's first Included GiB, at each capacity it had.
			for _, p := range parts[class] {
				out[class] += min(p.gib, a.Included) * usage.Overlap(p.from, p.to, w.Start, w.End).Hours()
			}
		case a == r.every:
			// Every class has an allowance of its own.
			for k, v := range pool(w, a, []string{class}, parts, uses.nodes) {
				out[k] += v
			}
		default:
			pooled[a] = append(pooled[a], class)
		}
	}
	for a, classes := range pooled {
		order := make([]string, 0, len(classes))
		for _, class := range a.StorageClasses {
			if len(parts[class]) > 0 {
				order = append(order, class)
			}
		}
		for k, v := range pool(w, a, order, parts, uses.nodes) {
			out[k] += v
		}
	}
	return out
}

// pool applies a node or cluster allowance shared by classes, in that
// order, within a window: at each instant, the allowance (Included, times
// the nodes billed then for a node allowance) covers what the first class
// holds, then what the next holds from what is left. The window is cut
// wherever a volume or a node starts or stops being billed or changes size,
// so each piece is constant.
func pool(w Window, a *Allowance, classes []string, parts map[string][]volumePart, nodes [][2]time.Time) map[string]float64 {
	cuts := []time.Time{w.Start, w.End}
	cut := func(at time.Time) {
		if at.After(w.Start) && at.Before(w.End) {
			cuts = append(cuts, at)
		}
	}
	for _, class := range classes {
		for _, p := range parts[class] {
			cut(p.from)
			cut(p.to)
		}
	}
	if a.Per == PerNode {
		for _, n := range nodes {
			cut(n[0])
			cut(n[1])
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	out := map[string]float64{}
	for i := 0; i+1 < len(cuts); i++ {
		from, to := cuts[i], cuts[i+1]
		if !to.After(from) {
			continue
		}
		left := a.Included
		if a.Per == PerNode {
			billed := 0
			for _, n := range nodes {
				if spans(n[0], n[1], from, to) {
					billed++
				}
			}
			left *= float64(billed)
		}
		hours := to.Sub(from).Hours()
		for _, class := range classes {
			if left <= 0 {
				break
			}
			held := 0.0
			for _, p := range parts[class] {
				if spans(p.from, p.to, from, to) {
					held += p.gib
				}
			}
			if take := min(held, left); take > 0 {
				out[class] += take * hours
				left -= take
			}
		}
	}
	return out
}

// spans reports whether [start, end) holds all of [from, to); a zero end is
// open.
func spans(start, end, from, to time.Time) bool {
	return !start.After(from) && (end.IsZero() || !end.Before(to))
}

// classesOf returns the storage classes of a map, sorted.
func classesOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// cover records that an allowance covers qty GiB-hours of the storage event
// tmpl identifies.
func (a *accumulator) cover(tmpl usage.Event, qty float64) {
	tmpl.ID = ""
	tmpl.WindowStart, tmpl.WindowEnd = a.window.Start.UTC(), a.window.End.UTC()
	a.covered[usage.DeriveID(&tmpl)] += qty
}

// settleAllowances bills the storage above the allowances: for each storage
// event of the window, what its allowances did not cover becomes a
// vcluster_storage_overage_gb_hours event with the same tenant, cluster,
// region and storage class. Storage of a class no allowance applies to is
// all overage, so with allowances configured every storage class is billed
// through the overage metric. Both events record what was applied:
// overage_gib_hours on the storage event, gross_gib_hours on the overage
// event, and the allowance (included_gib, allowance_per) on both. An event
// of zero overage is not made, as for every billed metric; the storage
// event says it was 0. It runs once per window, after every cluster is
// metered, so a storage event and what covers it are complete.
func (a *accumulator) settleAllowances(r *allowanceRules) {
	if r == nil {
		return
	}
	type overage struct {
		tmpl  usage.Event
		qty   float64
		props map[string]any
	}
	var overages []overage
	for _, k := range a.order {
		ev := a.events[k]
		if ev.Metric != usage.MetricStorageGBHours {
			continue
		}
		above := max(usage.Round(ev.Quantity-a.covered[k]), 0)
		props := map[string]any{"gross_gib_hours": usage.Round(ev.Quantity)}
		if al := r.of(ev.SKU); al != nil {
			for name, v := range al.properties() {
				props[name] = v
				ev.Properties[name] = v
			}
		}
		ev.Properties[usage.PropOverageGiBHours] = above
		if above > 0 {
			o := *ev
			o.Metric, o.Unit, o.ID, o.Quantity = usage.MetricStorageOverageGBHours, "", "", 0
			o.Dimensions, o.Properties = maps.Clone(ev.Dimensions), nil
			overages = append(overages, overage{tmpl: o, qty: above, props: props})
		}
	}
	for _, o := range overages {
		a.add(o.tmpl, o.qty, nil)
		key := o.tmpl
		key.WindowStart, key.WindowEnd = a.window.Start.UTC(), a.window.End.UTC()
		maps.Copy(a.events[usage.DeriveID(&key)].Properties, o.props)
	}
}
