// Package pause records when clusters were paused, so metering can stop
// billing capacity for exactly that time, and keeps the inventory of
// clusters whose own API is down while they are paused.
//
// A paused cluster's control plane is scaled to zero, the way `vcluster
// pause` does it, and its workloads are stopped. What remains is capacity
// that still exists: dedicated or private nodes, volumes and load
// balancers. Which of those keep billing while paused is policy
// (PAUSE_KEEP_BILLING); the spans recorded here let the collector cut the
// rest from the second vBilling verified the cluster's workloads were
// gone, in backfilled windows too. Billing never stops on anything less:
// a pause vBilling could not verify records no span, and a pause it only
// observed (ByExternal) stops only the control plane's hours. A span is
// verified only if it carries the mark BeginVerified records: v0.3.0
// opened its spans at its mark, before its drain and whatever the drain
// left running, so spans it wrote read back unverified.
package pause

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// Span is one pause. To is zero while the cluster is still paused.
type Span struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to,omitempty"`
	// By is who paused the cluster: ByVBilling, or ByExternal for a pause
	// vBilling observed but did not make (vcluster pause, an operator).
	By string `json:"by,omitempty"`
	// Drained marks a span vBilling opened once it had verified that the
	// cluster's workloads were gone (BeginVerified).
	Drained bool `json:"drained,omitempty"`

	// unproven is set on a span read back from the store that is vBilling's
	// but carries no Drained mark: v0.3.0 wrote it at its mark, before its
	// drain, or it predates By.
	unproven bool
}

// Who paused a cluster.
const (
	// ByVBilling spans are pauses vBilling made. Once verified (Drained:
	// it confirmed the cluster's workloads were gone before it recorded the
	// span), capacity stops billing for them (PAUSE_KEEP_BILLING decides
	// what keeps billing).
	ByVBilling = "vbilling"
	// ByExternal spans are pauses vBilling only observed: the control plane
	// is down, but nothing confirms the cluster's workloads stopped, so only
	// the control plane stops billing.
	ByExternal = "external"
)

// Verified reports whether a span stops billing more than the control
// plane. A span read from the store is verified only if vBilling marked it
// Drained when it opened it; a vBilling span without the mark (v0.3.0
// opened it at its mark, whatever its drain left running) is not.
func (s Span) Verified() bool { return s.By != ByExternal && !s.unproven }

// Unproven reports whether a span is vBilling's but was recorded without
// verifying anything, by an older version.
func (s Span) Unproven() bool { return s.unproven }

// UnmarshalJSON reads a span, marking vBilling spans without the Drained
// mark as unproven.
func (s *Span) UnmarshalJSON(b []byte) error {
	type plain Span // no methods: no recursion
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = Span(p)
	s.unproven = s.By != ByExternal && !s.Drained
	return nil
}

// Open reports whether the span has not ended.
func (s Span) Open() bool { return s.To.IsZero() }

// Store keeps pause spans per cluster (external ID). It is safe for
// concurrent use.
type Store struct {
	mu      sync.Mutex
	kv      *kv.Store
	snapDir string
}

// Open loads the span store under dir (DATA_DIR). An empty dir keeps
// everything in memory.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return &Store{kv: kv.Memory()}, nil
	}
	s, err := kv.Open(filepath.Join(dir, "pause-spans.json"))
	if err != nil {
		return nil, err
	}
	snap := filepath.Join(dir, "paused-inventory")
	if err := os.MkdirAll(snap, 0o750); err != nil {
		return nil, err
	}
	return &Store{kv: s, snapDir: snap}, nil
}

// Memory returns an in-memory store.
func Memory() *Store { return &Store{kv: kv.Memory()} }

func (s *Store) load(id string) []Span {
	raw, ok := s.kv.Get(id)
	if !ok {
		return nil
	}
	var spans []Span
	if json.Unmarshal([]byte(raw), &spans) != nil {
		return nil
	}
	return spans
}

func (s *Store) save(id string, spans []Span) error {
	if len(spans) == 0 {
		return s.kv.Delete(id)
	}
	b, err := json.Marshal(spans)
	if err != nil {
		return err
	}
	return s.kv.Set(id, string(b))
}

// Begin opens a pause for a cluster at the given time. It does nothing if
// one is already open, and reports whether it opened one. The span carries
// no Drained mark, so it is never verified: vBilling's verified pauses use
// BeginVerified.
func (s *Store) Begin(id string, at time.Time, by string) (bool, error) {
	return s.begin(id, Span{From: at, By: by})
}

// BeginVerified opens a pause vBilling verified (it confirmed the
// cluster's workloads were gone), marked Drained. It does nothing if one is
// already open, and reports whether it opened one.
func (s *Store) BeginVerified(id string, at time.Time) (bool, error) {
	return s.begin(id, Span{From: at, By: ByVBilling, Drained: true})
}

func (s *Store) begin(id string, sp Span) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spans := s.load(id)
	if n := len(spans); n > 0 && spans[n-1].Open() {
		return false, nil
	}
	sp.From = sp.From.UTC()
	if n := len(spans); n > 0 && sp.From.Before(spans[n-1].To) {
		sp.From = spans[n-1].To // spans never overlap
	}
	return true, s.save(id, append(spans, sp))
}

// End closes the open pause of a cluster at the given time, and reports
// whether one was open.
func (s *Store) End(id string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spans := s.load(id)
	n := len(spans)
	if n == 0 || !spans[n-1].Open() {
		return false, nil
	}
	at = at.UTC()
	if at.Before(spans[n-1].From) {
		at = spans[n-1].From
	}
	spans[n-1].To = at
	return true, s.save(id, spans)
}

// Discard drops the open pause of a cluster, as if it had never begun, and
// reports whether one was open. A pause that turned out not to hold (the
// cluster ran on) must not stop billing for any of its time.
func (s *Store) Discard(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spans := s.load(id)
	n := len(spans)
	if n == 0 || !spans[n-1].Open() {
		return false, nil
	}
	return true, s.save(id, spans[:n-1])
}

// Spans returns a cluster's pauses, oldest first.
func (s *Store) Spans(id string) []Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

// Current returns the open pause of a cluster, if any.
func (s *Store) Current(id string) (Span, bool) {
	spans := s.Spans(id)
	if n := len(spans); n > 0 && spans[n-1].Open() {
		return spans[n-1], true
	}
	return Span{}, false
}

// All returns every cluster's spans.
func (s *Store) All() map[string][]Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]Span{}
	for id, raw := range s.kv.WithPrefix("") {
		if strings.HasPrefix(id, "\x00") {
			continue // an inventory kept in memory
		}
		var spans []Span
		if json.Unmarshal([]byte(raw), &spans) == nil {
			out[id] = spans
		}
	}
	return out
}

// Prune drops pauses that ended before cutoff. Windows older than that are
// never metered again.
func (s *Store) Prune(cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for id, raw := range s.kv.WithPrefix("") {
		if strings.HasPrefix(id, "\x00") {
			continue
		}
		var spans []Span
		if json.Unmarshal([]byte(raw), &spans) != nil {
			continue
		}
		kept := spans[:0]
		for _, sp := range spans {
			if sp.Open() || !sp.To.Before(cutoff) {
				kept = append(kept, sp)
			}
		}
		if len(kept) != len(spans) {
			errs = append(errs, s.save(id, kept))
		}
	}
	return errors.Join(errs...)
}

// Overlap returns how much of [start, end) inside the window [ws, we) the
// cluster was paused. A zero end means "still exists".
func Overlap(spans []Span, start, end, ws, we time.Time) time.Duration {
	if !start.IsZero() && start.After(ws) {
		ws = start
	}
	if !end.IsZero() && end.Before(we) {
		we = end
	}
	if !we.After(ws) {
		return 0
	}
	var d time.Duration
	for _, sp := range spans {
		from, to := sp.From, sp.To
		if from.Before(ws) {
			from = ws
		}
		if to.IsZero() || to.After(we) {
			to = we
		}
		if to.After(from) {
			d += to.Sub(from)
		}
	}
	return d
}

// Outside returns the parts of [start, end) that fall outside every span,
// in order. A zero end means "still running"; the last part is then open
// (zero end) unless a span that is still open covers it.
func Outside(spans []Span, start, end time.Time) [][2]time.Time {
	sorted := append([]Span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].From.Before(sorted[j].From) })
	var out [][2]time.Time
	from := start
	for _, sp := range sorted {
		if !end.IsZero() && !sp.From.Before(end) {
			break
		}
		if !sp.To.IsZero() && !sp.To.After(from) {
			continue // ended before this part begins
		}
		if sp.From.After(from) {
			out = append(out, [2]time.Time{from, sp.From})
		}
		if sp.To.IsZero() {
			return out // paused from here on
		}
		from = sp.To
	}
	if end.IsZero() || end.After(from) {
		out = append(out, [2]time.Time{from, end})
	}
	return out
}

// Keep is the set of capacity kinds that keep billing while paused.
type Keep map[string]bool

// Capacity kinds for PAUSE_KEEP_BILLING.
const (
	KeepStorage       = "storage"
	KeepLoadBalancers = "load_balancers"
	KeepNodes         = "nodes"
	KeepControlPlane  = "control_plane"
	// KeepWorkloads bills a paused cluster's pods until they actually stop.
	// Without it, their usage stops at the second of the pause: stopping a
	// GPU workload can take its whole termination grace period (vLLM's API
	// server, for one, is killed only when the grace period ends), and that
	// time is the pause's doing, not the tenant's.
	KeepWorkloads = "workloads"
)

// DefaultKeep keeps billing what a paused cluster still occupies for the
// operator and the customer gets back on resume: volumes and load balancer
// addresses. Nodes, workloads and the control plane stop billing.
const DefaultKeep = KeepStorage + "," + KeepLoadBalancers

// ParseKeep parses PAUSE_KEEP_BILLING ("storage,load_balancers", "none").
func ParseKeep(spec string) (Keep, error) {
	out := Keep{}
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "none" {
		return out, nil
	}
	valid := []string{KeepStorage, KeepLoadBalancers, KeepNodes, KeepControlPlane, KeepWorkloads}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		ok := false
		for _, v := range valid {
			ok = ok || v == item
		}
		if !ok {
			return nil, fmt.Errorf("PAUSE_KEEP_BILLING: unknown %q (use %s, or none)", item, strings.Join(valid, ", "))
		}
		out[item] = true
	}
	return out, nil
}

// String lists the kinds, sorted.
func (k Keep) String() string {
	var out []string
	for v := range k {
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ",")
}

// SaveInventory stores a paused cluster's inventory (its own nodes,
// volumes and load balancers, as JSON), read just before its control plane
// stopped. While paused nothing in it can change, so it is metered from
// this copy.
func (s *Store) SaveInventory(id string, inv []byte) error {
	if s.snapDir == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.kv.Set("\x00inv/"+id, string(inv))
	}
	path := filepath.Join(s.snapDir, safeName(id)+".json")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(inv); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Inventory returns the stored inventory of a paused cluster.
func (s *Store) Inventory(id string) ([]byte, bool) {
	if s.snapDir == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		v, ok := s.kv.Get("\x00inv/" + id)
		return []byte(v), ok
	}
	b, err := os.ReadFile(filepath.Join(s.snapDir, safeName(id)+".json"))
	return b, err == nil
}

// DropInventory removes a cluster's stored inventory after it resumed.
func (s *Store) DropInventory(id string) error {
	if s.snapDir == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.kv.Delete("\x00inv/" + id)
	}
	err := os.Remove(filepath.Join(s.snapDir, safeName(id)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ':' {
			return '_'
		}
		return r
	}, id)
}
