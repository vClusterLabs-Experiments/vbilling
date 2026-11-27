package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Escalations (ENFORCEMENT_ESCALATIONS) are grace periods: a billing hold
// that stays at one state for a set time is raised to a more severe one, for
// example delinquent to suspended after 72 hours, and suspended to paused 7
// days after that.
//
// Only the billing hold escalates, whoever set it: a webhook, a Metronome
// threshold notification, or a PUT that names "hold": "billing". A hold an
// operator sets under any other name (operator, support, abuse) never
// escalates: the operator chose its state.
//
// The clock is the billing hold's since, when the hold reached its state.
// It is stored with the hold, so a restart keeps it, and a hold past its
// time when vBilling starts escalates at the first pass. An escalation sets
// the hold's since to when it applied, so the next rule counts from there.
// A payment that lifts the hold ends the escalation. A billing signal that
// lowers the hold, or raises it above the escalated state, starts the
// clock of the new state. A signal at the same state, or a lesser one the
// hold keeps, changes neither the clock nor the rule that raised the hold.

// Escalation is one rule of ENFORCEMENT_ESCALATIONS: a billing hold that has
// been at From for After is raised to To.
type Escalation struct {
	From  State
	After time.Duration
	To    State
	after string // After as written
}

// String writes the rule as ENFORCEMENT_ESCALATIONS does: from:after:to.
func (r Escalation) String() string {
	return string(r.From) + ":" + r.afterText() + ":" + string(r.To)
}

// MarshalJSON writes {"from", "after", "to"}, after as written.
func (r Escalation) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		From  State  `json:"from"`
		After string `json:"after"`
		To    State  `json:"to"`
	}{r.From, r.afterText(), r.To})
}

// afterText is After as written, or in its largest whole unit.
func (r Escalation) afterText() string {
	if r.after != "" {
		return r.after
	}
	return formatAfter(r.After)
}

// NextEscalation is when a billing hold escalates next, to what, and by
// which rule. It is computed from the hold's since and the rules, never
// stored.
type NextEscalation struct {
	To   State     `json:"to"`
	At   time.Time `json:"at"`
	Rule string    `json:"rule"`
}

// MinEscalationAfter is the shortest grace an escalation rule takes.
const MinEscalationAfter = time.Minute

// EscalateInterval is how often Escalate looks for billing holds past their
// time: a hold escalates within this long of its escalation's at.
const EscalateInterval = 10 * time.Second

const escalationsMetric, escalationsHelp = "vbilling_enforcement_escalations_total", "Billing holds that ENFORCEMENT_ESCALATIONS raised, by the state they left and the state they reached."

// ParseEscalations parses ENFORCEMENT_ESCALATIONS: "from:after:to,...", for
// example "delinquent:72h:suspended,suspended:7d:paused". after takes d, h,
// m and s ("72h", "7d", "1d12h") and is at least a minute. Only delinquent
// and suspended escalate, by one rule each, to a more severe state.
func ParseEscalations(spec string) ([]Escalation, error) {
	var out []Escalation
	byFrom := map[State]Escalation{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		r, err := parseEscalation(item)
		if err != nil {
			return nil, err
		}
		if prev, dup := byFrom[r.From]; dup {
			return nil, fmt.Errorf("escalations %q and %q both start at %s: give each state one rule", prev.String(), r.String(), r.From)
		}
		byFrom[r.From] = r
		out = append(out, r)
	}
	for _, r := range out {
		seen := map[State]bool{r.From: true}
		path := []string{string(r.From)}
		for s := r.To; ; {
			path = append(path, string(s))
			if seen[s] {
				return nil, fmt.Errorf("escalations loop: %s", strings.Join(path, " -> "))
			}
			seen[s] = true
			next, ok := byFrom[s]
			if !ok {
				break
			}
			s = next.To
		}
	}
	for _, r := range out {
		if r.From != Delinquent && r.From != Suspended {
			return nil, fmt.Errorf("escalation %q: only delinquent and suspended escalate", r.String())
		}
		if severity[r.To] <= severity[r.From] {
			return nil, fmt.Errorf("escalation %q: %s is not more severe than %s", r.String(), r.To, r.From)
		}
	}
	return out, nil
}

// parseEscalation parses one from:after:to.
func parseEscalation(item string) (Escalation, error) {
	parts := strings.Split(item, ":")
	if len(parts) != 3 {
		return Escalation{}, fmt.Errorf("escalation %q must be from:after:to, for example delinquent:72h:suspended", item)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	r := Escalation{From: State(parts[0]), after: parts[1], To: State(parts[2])}
	for _, s := range []State{r.From, r.To} {
		if !s.valid() {
			return Escalation{}, fmt.Errorf("escalation %q: unknown state %q (states: active, warning, delinquent, suspended, paused)", item, s)
		}
	}
	d, err := parseAfter(r.after)
	if err != nil {
		return Escalation{}, fmt.Errorf("escalation %q: after %q must be a duration in d, h, m or s, such as 72h or 7d", item, r.after)
	}
	if d < MinEscalationAfter {
		return Escalation{}, fmt.Errorf("escalation %q: after %q must be at least 1m", item, r.after)
	}
	r.After = d
	return r, nil
}

// maxAfter keeps a grace period well inside what a time.Duration holds.
const maxAfter = 100 * 365 * 24 * time.Hour

// parseAfter parses a duration of numbers with the units d, h, m and s, as
// in 72h, 7d, 1d12h or 1.5h.
func parseAfter(s string) (time.Duration, error) {
	units := map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}
	if s == "" {
		return 0, errors.New("empty duration")
	}
	var total float64
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
			j++
		}
		if j == i || j == len(s) {
			return 0, fmt.Errorf("duration %q: a number needs a unit", s)
		}
		unit, ok := units[s[j]]
		if !ok {
			return 0, fmt.Errorf("duration %q: unknown unit %q", s, s[j])
		}
		n, err := strconv.ParseFloat(s[i:j], 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		total += n * float64(unit)
		if total > float64(maxAfter) {
			return 0, fmt.Errorf("duration %q is too long", s)
		}
		i = j + 1
	}
	return time.Duration(total), nil
}

// formatAfter writes a duration in the largest unit that divides it.
func formatAfter(d time.Duration) string {
	switch {
	case d > 0 && d%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d > 0 && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d > 0 && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}

// Escalations returns the escalation rules, as configured.
func (e *Enforcer) Escalations() []Escalation {
	return append([]Escalation(nil), e.rules...)
}

// nextEscalation returns the rule a billing hold escalates by next, and
// when; ok is false when no rule applies to its state.
func (e *Enforcer) nextEscalation(h Hold) (r Escalation, at time.Time, ok bool) {
	r, ok = e.escalations[h.State]
	if !ok || h.Since.IsZero() {
		return Escalation{}, time.Time{}, false
	}
	return r, h.Since.Add(r.After).UTC(), true
}

// withEscalation adds to a state what it shows but does not store: when its
// billing hold escalates next, on the hold, and, when that raises the
// tenant's state, on the state too. Another hold can keep the tenant above
// what the billing hold escalates to.
func (e *Enforcer) withEscalation(ts TenantState) TenantState {
	h, held := ts.Holds[HoldBilling]
	if !held {
		return ts
	}
	r, at, ok := e.nextEscalation(h)
	if !ok {
		return ts
	}
	next := &NextEscalation{To: r.To, At: at, Rule: r.String()}
	holds := make(map[string]Hold, len(ts.Holds))
	for k, v := range ts.Holds {
		holds[k] = v
	}
	h.Escalation = next
	holds[HoldBilling] = h
	ts.Holds = holds
	if severity[r.To] > severity[orActive(ts.State)] {
		ts.Escalation = next
	}
	return ts
}

// Escalate raises every billing hold that has been at a state for as long
// as its escalation rule allows (ENFORCEMENT_ESCALATIONS). Each change is
// stored, logged, counted in vbilling_enforcement_escalations_total and
// reflected on the tenant's clusters like any other: what it does there is
// ENFORCEMENT_MODE's. A hold that changed since it was read is left alone.
func (e *Enforcer) Escalate(ctx context.Context) {
	if len(e.escalations) == 0 {
		return
	}
	for _, ts := range e.States() {
		h, held := ts.Holds[HoldBilling]
		if !held {
			continue
		}
		r, at, ok := e.nextEscalation(h)
		if !ok || e.opts.Now().Before(at) {
			continue
		}
		reason := fmt.Sprintf("%s for %s: %s", r.From, r.afterText(), h.Reason)
		next, prev, res, err := e.store(ts.Tenant, update{hold: HoldBilling, state: r.To, reason: reason, escalate: &r, since: h.Since})
		if err != nil {
			log.Printf("[enforcement] tenant %s: escalation %s: %v", ts.Tenant, r, err)
			continue
		}
		if res.kind != outChanged {
			continue // the hold changed since it was read: the next pass decides again
		}
		e.opts.Telemetry.Add(escalationsMetric, escalationsHelp, map[string]string{"from": string(r.From), "to": string(r.To)}, 1)
		if prev.State == next.State {
			// The tenant's state comes from another hold: reflectState
			// logs no change, so the escalation is logged here.
			log.Printf("[enforcement] tenant %s: the billing hold escalated %s -> %s (escalation %s, %s since %s); the state stays %s from the %s hold",
				ts.Tenant, r.From, r.To, r, r.From, h.Since.UTC().Format(time.RFC3339), next.State, next.Hold)
		}
		if err := e.reflectState(ctx, prev, next, HoldBilling, "escalation", r.String()); err != nil {
			log.Printf("[enforcement] tenant %s: escalation %s: %v", ts.Tenant, r, err)
		}
	}
}
