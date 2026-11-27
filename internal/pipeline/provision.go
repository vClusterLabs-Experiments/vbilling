package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Tenants a destination cannot set up.
//
// A destination sees a tenant's usage only once the tenant is set up there
// (EnsureTenant: the customer, its subscription or contract). When that
// fails for one tenant, only that tenant waits:
//
//   - A retryable failure (a quota, an outage of the customer API, anything
//     that is not destinations.Permanent) parks the tenant's events for the
//     destination in the held store, under ParkKey(tenant, series). Every
//     other tenant's events are delivered and the cursor moves on.
//   - A permanent rejection means the destination will not take the tenant as
//     it is, and retrying the same call cannot help. Adapters return it when
//     the tenant's usage can be delivered anyway (the customer exists but its
//     plan cannot be subscribed, a receiver refuses the tenant notice), so
//     the usage is still sent, as it always was. What the destination then
//     refuses of that tenant is parked with the tenant instead of being
//     dead-lettered at once.
//
// Either way the tenant is retried on its own, with backoff, for as long as
// it has usage for the destination, and reported: a failing health source
// per tenant and destination, vbilling_tenants_blocked and
// vbilling_tenant_provisioning_failures_total. Once it is set up, its parked
// events ship in ledger order before anything newer of the tenant.
//
// Parked events do not wait forever: one that has been parked for MaxHold
// (the pricing gate's limit) is dead-lettered with the provisioning error,
// where it stays replayable. That needs the destination to work otherwise:
// while it accepts nothing from anyone it is down, and nothing is
// dead-lettered for being late, as nothing else is.

const (
	// blockIdle is how long after its last usage a tenant that could not be
	// set up is still retried. Without usage on its way there is nothing to
	// set it up for.
	blockIdle = time.Hour
	// blockForget is when an idle one is forgotten: its next usage starts
	// over with a first attempt.
	blockForget = 6 * time.Hour
)

// tenantBlock is a tenant a destination could not set up.
type tenantBlock struct {
	tenant    string
	permanent bool      // the destination rejected the tenant for good: its usage is still sent
	err       string    // the destination's last error
	since     time.Time // first failure of this streak
	attempts  int
	backoff   time.Duration
	next      time.Time // next provisioning attempt
	seen      time.Time // when the tenant last had usage on its way to the destination
	earliest  time.Time // earliest window of that usage since the streak began
	latest    time.Time // earliest window of the last batch that carried its usage
	idle      bool      // reported as idle: none of its usage waits
}

// BlockedTenant describes a tenant a destination could not set up, for the
// API.
type BlockedTenant struct {
	Tenant   string    `json:"tenant"`
	Error    string    `json:"error,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Attempts int       `json:"attempts,omitempty"`
	// Permanent: the destination rejected the tenant for good (retrying the
	// same call cannot help), so its usage is still sent.
	Permanent   bool      `json:"permanent,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Parked events wait for the tenant; the oldest has waited since
	// ParkedSince and is dead-lettered at DeadLetterAt if the tenant is not
	// set up before.
	Parked       int       `json:"parked_events"`
	ParkedSince  time.Time `json:"parked_since,omitzero"`
	DeadLetterAt time.Time `json:"dead_letter_at,omitzero"`
}

// outcome is what became of a batch handed to dispatch.
type outcome struct {
	delivered int           // accepted by the destination
	dead      int           // refused for good and dead-lettered
	parked    int           // parked for a tenant that is not set up
	already   int           // delivered earlier by a release of parked events
	sent      []usage.Event // what was handed to the destination
}

// tenantWaitError stops a batch for a tenant that is not set up when there
// is no held store to park its events in (tools and tests; vBilling always
// has one): the batch waits for the tenant, as every batch did before
// tenants were parked.
type tenantWaitError struct{ tenant, err string }

func (e *tenantWaitError) Error() string { return "ensure tenant " + e.tenant + ": " + e.err }

// parkedGroup is what is parked for one tenant at a destination.
type parkedGroup struct {
	tenant string
	keys   []string
	count  int
	since  time.Time // when its oldest parked event was parked
	oldest time.Time // earliest window among its parked events
}

// parkedGroups lists what is parked for dest by tenant, the tenant with the
// oldest parked event first.
func (d *Dispatcher) parkedGroups(name string) []parkedGroup {
	if d.opts.Held == nil {
		return nil
	}
	var out []parkedGroup
	at := map[string]int{}
	for _, h := range d.opts.Held.ParkedSeries(name) {
		i, ok := at[h.Sample.Tenant]
		if !ok {
			i = len(out)
			at[h.Sample.Tenant] = i
			out = append(out, parkedGroup{tenant: h.Sample.Tenant, since: h.Since, oldest: h.Oldest})
		}
		g := &out[i]
		g.keys = append(g.keys, h.Series)
		g.count += h.Count
		if h.Since.Before(g.since) {
			g.since = h.Since
		}
		if !h.Oldest.IsZero() && (g.oldest.IsZero() || h.Oldest.Before(g.oldest)) {
			g.oldest = h.Oldest
		}
	}
	return out
}

// block returns a copy of what is known about a tenant dest could not set
// up, nil when nothing is.
func (d *Dispatcher) block(name, tenant string) *tenantBlock {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.blocks[name][tenant]; b != nil {
		cp := *b
		return &cp
	}
	return nil
}

// touch notes that a tenant that is not set up has usage on its way to dest:
// it stays worth retrying, and its tenant must be set up early enough to
// count the usage from first, the earliest window of this batch.
func (d *Dispatcher) touch(name, tenant string, first time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.blocks[name][tenant]
	if b == nil {
		return
	}
	b.seen, b.idle = time.Now(), false
	b.latest = first
	if !first.IsZero() && (b.earliest.IsZero() || first.Before(b.earliest)) {
		b.earliest = first
	}
}

// tenantReady reports whether a tenant is set up at dest as it is now.
func (d *Dispatcher) tenantReady(name, tenant string) bool {
	if d.opts.Tenants == nil {
		return true
	}
	t, _ := d.opts.Tenants.Get(tenant)
	return !d.opts.Tenants.NeedsEnsure(name, t)
}

func (d *Dispatcher) noteAccepted(name string) {
	d.mu.Lock()
	d.accepted[name] = time.Now()
	d.mu.Unlock()
}

// acceptedSince reports whether dest accepted an event or a tenant after t:
// proof that it works for someone.
func (d *Dispatcher) acceptedSince(name string, t time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.accepted[name].After(t)
}

// provision makes one attempt to set a tenant up at dest. It returns what is
// known about the tenant afterwards (nil once it is set up) and whether ctx
// ended during the attempt, which is not a failure of the tenant. usageStart
// is the earliest window of the usage the tenant is set up for; hasUsage
// says that usage of the tenant is on its way now. A caller that is about to
// park that usage passes report false and reports the failure itself, once
// the health entry can say what is parked.
func (d *Dispatcher) provision(ctx context.Context, dest destinations.Destination, tenant string, usageStart time.Time, hasUsage, report bool) (*tenantBlock, bool) {
	name := dest.Name()
	t, _ := d.opts.Tenants.Get(tenant)
	mu := d.provMu[name]
	mu.Lock()
	began := time.Now()
	err := dest.EnsureTenant(destinations.WithUsageStart(ctx, usageStart), t)
	took := time.Since(began)
	mu.Unlock()
	if err != nil && ctx.Err() != nil {
		return d.block(name, tenant), true
	}
	// Retries share the loop with delivery: after a slow attempt the next
	// retry waits four times as long, so tenants that cannot be set up never
	// take more than a fifth of a destination's time.
	d.mu.Lock()
	d.pace[name] = time.Now().Add(4 * took)
	d.mu.Unlock()
	if err == nil {
		d.opts.Tenants.MarkEnsured(name, t)
		d.noteAccepted(name)
		d.unblock(name, tenant)
		return nil, false
	}
	return d.blocked(name, tenant, err, usageStart, hasUsage, report), false
}

// blocked records a failed provisioning attempt and reports it.
func (d *Dispatcher) blocked(name, tenant string, err error, usageStart time.Time, hasUsage, report bool) *tenantBlock {
	now := time.Now()
	permanent := destinations.IsPermanent(err)
	d.mu.Lock()
	if d.blocks[name] == nil {
		d.blocks[name] = map[string]*tenantBlock{}
	}
	b := d.blocks[name][tenant]
	fresh := b == nil
	if fresh {
		b = &tenantBlock{tenant: tenant, since: now, seen: now, earliest: usageStart, latest: usageStart}
		d.blocks[name][tenant] = b
	}
	changed := fresh || b.permanent != permanent
	b.permanent, b.err = permanent, err.Error()
	b.attempts++
	b.backoff = min(max(2*b.backoff, d.opts.MinBackoff), d.opts.MaxBackoff)
	b.next = now.Add(b.backoff + time.Duration(rand.Int63n(int64(b.backoff)/5+1)))
	if hasUsage {
		b.seen, b.idle = now, false
		b.latest = usageStart
		if !usageStart.IsZero() && (b.earliest.IsZero() || usageStart.Before(b.earliest)) {
			b.earliest = usageStart
		}
	}
	cp := *b
	d.mu.Unlock()

	d.tel.Add("vbilling_tenant_provisioning_failures_total", "Failed attempts to set a tenant up at a destination (EnsureTenant).",
		map[string]string{"destination": name}, 1)
	if changed {
		switch {
		case permanent:
			log.Printf("[pipeline] %s: tenant %s rejected: %v. %s will not take the tenant as it is, so its usage is still sent; "+
				"events %s refuses of it are parked until the tenant is set up (retried with backoff)", name, tenant, err, name, name)
		case d.opts.Held == nil:
			log.Printf("[pipeline] %s: tenant %s could not be set up: %v. Delivery waits for it (no held store to park its usage in)", name, tenant, err)
		default:
			log.Printf("[pipeline] %s: tenant %s could not be set up: %v. Its usage for %s is parked and ships in order once the tenant is set up; "+
				"other tenants are not affected (retried with backoff)", name, tenant, err, name)
		}
	}
	if report {
		d.reportBlock(name, cp)
	}
	d.publishBlocked(name)
	return &cp
}

// unblock forgets a tenant's provisioning failure: the tenant is set up.
func (d *Dispatcher) unblock(name, tenant string) {
	d.mu.Lock()
	b := d.blocks[name][tenant]
	delete(d.blocks[name], tenant)
	d.mu.Unlock()
	if b == nil {
		return
	}
	log.Printf("[pipeline] %s: tenant %s is set up after %d failed attempt(s) over %s", name, tenant, b.attempts,
		time.Since(b.since).Round(time.Second))
	if d.opts.Health != nil {
		d.opts.Health.OK(health.Name("provisioning", name, tenant), "provisioning", tenant, 1)
	}
	d.publishBlocked(name)
}

// reportBlock publishes a tenant's provisioning failure to the health
// registry, with what waits for it and for how long it still can.
func (d *Dispatcher) reportBlock(name string, b tenantBlock) {
	if d.opts.Health == nil {
		return
	}
	msg := fmt.Sprintf("%s could not set up tenant %s: %s (%d attempt(s) since %s)", name, b.tenant, b.err, b.attempts, b.since.UTC().Format(time.RFC3339))
	hint := "fix what " + name + " reports for this tenant; vBilling retries with backoff and other tenants are not affected"
	if b.permanent {
		msg += "; " + name + " rejected the tenant for good, so its usage is still sent, and events it refuses are parked"
	}
	for _, g := range d.parkedGroups(name) {
		if g.tenant != b.tenant {
			continue
		}
		msg += fmt.Sprintf("; %d event(s) parked since %s", g.count, g.since.UTC().Format(time.RFC3339))
		if mh := d.maxHold(); mh > 0 {
			at := g.since.Add(mh)
			switch left := time.Until(at); {
			case !d.acceptedSince(name, b.since):
				msg += fmt.Sprintf("; %s has accepted nothing since, so nothing is dead-lettered until it works for another tenant", name)
			case left > 0:
				msg += fmt.Sprintf("; the oldest is dead-lettered in %s (at %s) unless the tenant is set up before", left.Round(time.Minute), at.UTC().Format(time.RFC3339))
			default:
				msg += "; events parked longer than " + mh.String() + " are being dead-lettered"
			}
			hint += fmt.Sprintf(". Parked usage ships in order once the tenant is set up; after %s parked it is dead-lettered and stays replayable "+
				"(POST /api/v1/destinations/%s/dead-letters/replay)", mh, name)
		}
	}
	d.opts.Health.Fail(health.Name("provisioning", name, b.tenant), "provisioning", b.tenant, errors.New(msg), hint)
}

// blockedTenants counts the tenants dest could not set up: those with a
// failure outstanding, and those with parked events.
func (d *Dispatcher) blockedTenants(name string) int {
	seen := map[string]bool{}
	for _, g := range d.parkedGroups(name) {
		seen[g.tenant] = true
	}
	d.mu.Lock()
	for id, b := range d.blocks[name] {
		if !b.idle {
			seen[id] = true
		}
	}
	d.mu.Unlock()
	return len(seen)
}

func (d *Dispatcher) publishBlocked(name string) {
	d.tel.Set("vbilling_tenants_blocked", "Tenants a destination could not set up (EnsureTenant keeps failing): their usage for it is parked, "+
		"or still sent when the destination rejected the tenant for good.", map[string]string{"destination": name}, float64(d.blockedTenants(name)))
}

// Blocked lists the tenants a destination could not set up, with what is
// parked for each, the longest waiting first.
func (d *Dispatcher) Blocked(name string) []BlockedTenant {
	groups := d.parkedGroups(name)
	var out []BlockedTenant
	at := map[string]int{}
	for _, g := range groups {
		row := BlockedTenant{Tenant: g.tenant, Parked: g.count, ParkedSince: g.since}
		if mh := d.maxHold(); mh > 0 {
			row.DeadLetterAt = g.since.Add(mh)
		}
		at[g.tenant] = len(out)
		out = append(out, row)
	}
	d.mu.Lock()
	for id, b := range d.blocks[name] {
		i, ok := at[id]
		if !ok {
			if b.idle {
				continue
			}
			i = len(out)
			out = append(out, BlockedTenant{Tenant: id})
		}
		out[i].Error, out[i].Since, out[i].Attempts, out[i].Permanent, out[i].NextAttempt = b.err, b.since.UTC(), b.attempts, b.permanent, b.next.UTC()
	}
	d.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Parked > 0) != (b.Parked > 0) {
			return a.Parked > 0
		}
		if !a.ParkedSince.Equal(b.ParkedSince) {
			return a.ParkedSince.Before(b.ParkedSince)
		}
		return a.Tenant < b.Tenant
	})
	return out
}

// firstWindows returns each tenant's earliest window among events, and the
// tenants in order of first appearance: a billing period that starts when
// the customer is subscribed must start early enough to count the usage.
func firstWindows(n int, event func(i int) *usage.Event) (map[string]time.Time, []string) {
	first := map[string]time.Time{}
	var order []string
	for i := 0; i < n; i++ {
		e := event(i)
		at, seen := first[e.Tenant]
		if !seen {
			order = append(order, e.Tenant)
		}
		if !seen || e.WindowStart.Before(at) {
			first[e.Tenant] = e.WindowStart
		}
	}
	return first, order
}

// setUpHeldTenants gives the tenants of events the pricing gate holds their
// first provisioning attempt. A failure holds nothing up: the tenant is
// retried on its own, and its events are parked when the gate releases them.
func (d *Dispatcher) setUpHeldTenants(ctx context.Context, dest destinations.Destination, hold []HeldEntry) error {
	if d.opts.Tenants == nil {
		return nil
	}
	name := dest.Name()
	first, order := firstWindows(len(hold), func(i int) *usage.Event { return &hold[i].Event })
	for _, id := range order {
		if d.tenantReady(name, id) {
			continue
		}
		if d.block(name, id) != nil {
			d.touch(name, id, first[id])
			continue
		}
		if _, interrupted := d.provision(ctx, dest, id, first[id], true, true); interrupted {
			return ctx.Err()
		}
	}
	return nil
}

// gateTenants decides, tenant by tenant, which of a batch's events may be
// sent to dest now, and parks the others durably. A tenant seen for the
// first time (or changed since it was set up) gets its provisioning attempt
// here; one that failed before is retried by tendTenants, not here.
func (d *Dispatcher) gateTenants(ctx context.Context, dest destinations.Destination, items []item) ([]item, outcome, error) {
	var out outcome
	if d.opts.Tenants == nil || len(items) == 0 {
		return items, out, nil
	}
	name := dest.Name()
	first, order := firstWindows(len(items), func(i int) *usage.Event { return &items[i].ev })
	parkedOf := map[string]int{}
	for _, g := range d.parkedGroups(name) {
		parkedOf[g.tenant] = g.count
	}
	const queued = "queued behind parked events of the tenant"
	hold := map[string]string{} // tenant -> why its events are parked
	var failed []string         // tenants whose first attempt failed in this pass
	for _, id := range order {
		if d.tenantReady(name, id) {
			d.unblock(name, id)
			if parkedOf[id] > 0 {
				hold[id] = queued // until tendTenants has shipped what is parked: ledger order
			}
			continue
		}
		b := d.block(name, id)
		if b == nil {
			var interrupted bool
			if b, interrupted = d.provision(ctx, dest, id, first[id], true, false); interrupted {
				return nil, out, ctx.Err()
			}
			if b != nil {
				failed = append(failed, id)
			}
		} else {
			d.touch(name, id, first[id])
		}
		switch {
		case b == nil: // set up just now
			if parkedOf[id] > 0 {
				hold[id] = queued
			}
		case b.permanent:
			// Rejected for good: its usage is still sent (see the top of this file).
		case d.opts.Held == nil:
			return nil, out, &tenantWaitError{tenant: id, err: b.err}
		default:
			hold[id] = "tenant " + id + " is not set up at " + name + ": " + b.err
		}
	}

	now := time.Now().UTC()
	ship := items[:0:0]
	var park []HeldEntry
	for _, it := range items {
		key := ParkKey(it.ev.Tenant, it.series)
		if it.seq <= d.shipped.get(name, key) {
			out.already++ // delivered by a release of the tenant's parked events
			continue
		}
		if reason, ok := hold[it.ev.Tenant]; ok {
			park = append(park, HeldEntry{Seq: it.seq, At: now, Series: key, Reason: reason, Event: it.ev})
			continue
		}
		ship = append(ship, it)
	}
	if len(park) > 0 {
		if err := d.parkEntries(name, park); err != nil {
			return nil, out, err
		}
		out.parked = len(park)
	}
	for _, id := range failed {
		if b := d.block(name, id); b != nil {
			d.reportBlock(name, *b) // now that its events are parked: with how many, and until when
		}
	}
	return ship, out, nil
}

// parkEntries writes parked events durably, before a cursor or a held series
// moves past them.
func (d *Dispatcher) parkEntries(name string, park []HeldEntry) error {
	added, err := d.opts.Held.Add(name, park)
	if err != nil {
		return fmt.Errorf("park events: %w", err)
	}
	if added > 0 {
		log.Printf("[pipeline] %s: parked %d event(s): %s", name, added, park[0].Reason)
	}
	d.publishHeld(name)
	d.publishBlocked(name)
	return nil
}

// dispatch hands a batch to a destination: the events of tenants that are
// not set up there are parked, the others are sent, and what the
// destination refuses for good is dead-lettered (or parked, for a tenant it
// rejected). When it returns without an error every event of the batch is
// safe: delivered, parked or dead-lettered.
func (d *Dispatcher) dispatch(ctx context.Context, dest destinations.Destination, items []item) (outcome, error) {
	ship, out, err := d.gateTenants(ctx, dest, items)
	if err != nil || len(ship) == 0 {
		return out, err
	}
	name := dest.Name()
	events := eventsOf(ship)
	refused, err := d.send(ctx, dest, events)
	if err != nil {
		return out, err
	}
	if len(refused) < len(events) {
		d.noteAccepted(name)
	}
	now := time.Now().UTC()
	var park []HeldEntry
	for _, it := range ship {
		reason, ok := refused[it.ev.ID]
		if !ok {
			continue
		}
		if d.opts.Held != nil && d.block(name, it.ev.Tenant) != nil {
			// The destination rejected the tenant and now this event: it
			// most likely refuses it because the tenant is not set up.
			park = append(park, HeldEntry{Seq: it.seq, At: now, Series: ParkKey(it.ev.Tenant, it.series), Event: it.ev,
				Reason: "refused by " + name + " while tenant " + it.ev.Tenant + " is not set up there: " + reason})
			continue
		}
		if err := d.deadLetter(name, it.ev, reason); err != nil {
			return out, err
		}
		out.dead++
	}
	if len(park) > 0 {
		if err := d.parkEntries(name, park); err != nil {
			return out, err
		}
		out.parked += len(park)
	}
	out.delivered = len(events) - len(refused)
	out.sent = events
	return out, nil
}

// retryDue reports whether a tenant that is not set up may be tried now: it
// was never tried by this process, or its backoff has passed.
func (d *Dispatcher) retryDue(name, tenant string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if now.Before(d.pace[name]) {
		return false
	}
	b := d.blocks[name][tenant]
	return b == nil || !now.Before(b.next)
}

// setUpFrom is the earliest window a tenant that is not set up must be set
// up for: its parked events and, while its usage waits elsewhere (held at
// the pricing gate), everything seen since it failed first. A tenant the
// destination rejected for good has its usage sent, so it is set up for the
// batch that carried its usage last, as before.
func setUpFrom(b *tenantBlock, parkedOldest time.Time) time.Time {
	var start time.Time
	if b != nil {
		start = b.earliest
		if b.permanent {
			start = b.latest
		}
	}
	if !parkedOldest.IsZero() && (start.IsZero() || parkedOldest.Before(start)) {
		start = parkedOldest
	}
	return start
}

// tendTenants looks after the tenants dest could not set up: parked events
// of tenants that are set up now ship, in ledger order; at most one tenant
// that is not gets its next provisioning attempt; and events that have been
// parked for too long are dead-lettered. An error is a failed delivery of
// parked events: the caller retries after a backoff.
func (d *Dispatcher) tendTenants(ctx context.Context, dest destinations.Destination) error {
	name := dest.Name()
	groups := d.parkedGroups(name)
	attempted := false // one provisioning retry per pass: delivery comes first
	parked := map[string]bool{}
	for _, g := range groups {
		parked[g.tenant] = true
		ready := d.tenantReady(name, g.tenant)
		fresh := false // tried in this pass
		if !ready && !attempted && d.retryDue(name, g.tenant) {
			attempted, fresh = true, true
			b, interrupted := d.provision(ctx, dest, g.tenant, setUpFrom(d.block(name, g.tenant), g.oldest), false, true)
			if interrupted {
				return ctx.Err()
			}
			ready = b == nil
		}
		if ready {
			d.unblock(name, g.tenant)
			if err := d.releaseTenant(ctx, dest, g); err != nil {
				return err
			}
			continue
		}
		if err := d.expireParked(ctx, dest, g, &attempted, fresh); err != nil {
			return err
		}
	}

	// Tenants with nothing parked: their usage is still sent (rejected for
	// good) or waits at the pricing gate. The most overdue one is tried.
	now := time.Now()
	var due *tenantBlock
	d.mu.Lock()
	for id, b := range d.blocks[name] {
		if parked[id] {
			continue
		}
		switch idle := now.Sub(b.seen); {
		case idle >= blockForget:
			delete(d.blocks[name], id)
		case idle >= blockIdle:
			if !b.idle {
				b.idle = true
				cp := *b
				defer d.reportIdle(name, cp)
			}
		case !attempted && !now.Before(b.next) && !now.Before(d.pace[name]) && (due == nil || b.next.Before(due.next)):
			cp := *b
			due = &cp
		}
	}
	d.mu.Unlock()
	if due != nil {
		if _, interrupted := d.provision(ctx, dest, due.tenant, setUpFrom(due, time.Time{}), false, true); interrupted {
			return ctx.Err()
		}
	}
	d.publishBlocked(name)
	return nil
}

// reportIdle says that a tenant that could not be set up has no usage
// waiting any more, so it is no longer retried (and no longer a problem).
func (d *Dispatcher) reportIdle(name string, b tenantBlock) {
	log.Printf("[pipeline] %s: tenant %s could not be set up (%s) and has had no usage for %s in %s: it is tried again when it has",
		name, b.tenant, b.err, name, blockIdle)
	if d.opts.Health != nil {
		d.opts.Health.Set(health.Name("provisioning", name, b.tenant), "provisioning", b.tenant, health.Paused,
			fmt.Sprintf("%s could not set up tenant %s (%s); none of its usage waits for %s, so it is tried again when it has usage", name, b.tenant, b.err, name), "")
	}
}

// idleFor is how long a destination with nothing to deliver waits before it
// looks again: the idle poll, or less when a tenant's provisioning retry is
// due sooner.
func (d *Dispatcher) idleFor(name string) time.Duration {
	wait := d.opts.IdlePoll
	parked := map[string]bool{}
	for _, g := range d.parkedGroups(name) {
		parked[g.tenant] = true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	pace := d.pace[name]
	consider := func(at time.Time) {
		if at.Before(pace) {
			at = pace
		}
		if until := at.Sub(now); until < wait {
			wait = max(until, time.Millisecond)
		}
	}
	for id, b := range d.blocks[name] {
		if parked[id] || now.Sub(b.seen) < blockIdle {
			consider(b.next)
		}
		delete(parked, id)
	}
	if len(parked) > 0 { // parked by an earlier run and not tried yet by this one
		consider(now)
	}
	return wait
}

// releaseTenant delivers the parked events of a tenant that is set up, in
// ledger order across its series, before anything newer of the tenant.
func (d *Dispatcher) releaseTenant(ctx context.Context, dest destinations.Destination, g parkedGroup) error {
	name := dest.Name()
	labels := map[string]string{"destination": name}
	total := 0
	for ctx.Err() == nil {
		// The oldest events of the tenant are among the first BatchSize of
		// each of its files, which are in ledger order each.
		var batch []HeldEntry
		for _, key := range g.keys {
			entries, _, err := d.opts.Held.Peek(name, key, d.opts.BatchSize)
			if err != nil {
				return err
			}
			batch = append(batch, entries...)
		}
		if len(batch) == 0 {
			break
		}
		sort.SliceStable(batch, func(i, j int) bool { return batch[i].Seq < batch[j].Seq })
		if len(batch) > d.opts.BatchSize {
			batch = batch[:d.opts.BatchSize]
		}
		events := make([]usage.Event, len(batch))
		for i := range batch {
			events[i] = batch[i].Event
		}
		refused, err := d.send(ctx, dest, events)
		if err != nil {
			return fmt.Errorf("release parked events of tenant %s: %w", g.tenant, err)
		}
		if len(refused) < len(events) {
			d.noteAccepted(name)
		}
		dead := 0
		for _, e := range batch {
			if reason, ok := refused[e.Event.ID]; ok {
				if err := d.deadLetter(name, e.Event, reason); err != nil {
					return err
				}
				dead++
			}
		}
		last, n := map[string]HeldEntry{}, map[string]int{}
		for _, e := range batch {
			last[e.Series] = e
			n[e.Series]++
		}
		for key, e := range last {
			if err := d.opts.Held.Release(name, key, e.Seq, n[key], e.end); err != nil {
				return fmt.Errorf("release parked events of tenant %s: %w", g.tenant, err)
			}
			d.shipped.set(name, key, e.Seq)
		}
		d.checkpointDelivery(dest, events)
		delivered := len(batch) - dead
		total += len(batch)
		d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(delivered))
		if dead > 0 {
			d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(dead))
		}
		d.update(name, func(s *Status) {
			s.Delivered += uint64(delivered)
			s.DeadLettered += uint64(dead)
			s.LastSuccess = time.Now().UTC()
			s.ConsecutiveFailures = 0
		})
		d.publishHeld(name)
	}
	if total > 0 {
		log.Printf("[pipeline] %s: released %d parked event(s) of tenant %s, in ledger order", name, total, g.tenant)
	}
	d.publishBlocked(name)
	return ctx.Err()
}

// expireParked dead-letters the events of a tenant that have been parked
// for longer than maxHold. Two things come first. The destination must have
// accepted something since the tenant failed first: while it accepts nothing
// from anyone it is down, and usage is never dead-lettered for an outage.
// And the tenant gets one more attempt right now (unless it just had one):
// an event is not dead-lettered for a failure that is over.
func (d *Dispatcher) expireParked(ctx context.Context, dest destinations.Destination, g parkedGroup, attempted *bool, fresh bool) error {
	mh := d.maxHold()
	if mh <= 0 {
		return nil
	}
	name := dest.Name()
	cutoff := time.Now().Add(-mh)
	if !g.since.Before(cutoff) {
		return nil
	}
	b := d.block(name, g.tenant)
	if b == nil || !d.acceptedSince(name, b.since) {
		return nil
	}
	if !fresh {
		d.mu.Lock()
		paced := time.Now().Before(d.pace[name])
		d.mu.Unlock()
		if *attempted || paced {
			return nil // its attempt comes in a later pass
		}
		*attempted = true
		var interrupted bool
		if b, interrupted = d.provision(ctx, dest, g.tenant, setUpFrom(b, g.oldest), false, true); interrupted {
			return ctx.Err()
		}
		if b == nil {
			return d.releaseTenant(ctx, dest, g)
		}
	}
	total := 0
	for _, key := range g.keys {
		for ctx.Err() == nil {
			through, err := d.opts.Held.ParkedBefore(name, key, cutoff)
			if err != nil {
				return err
			}
			entries, _, err := d.opts.Held.Peek(name, key, d.opts.BatchSize)
			if err != nil {
				return err
			}
			n := 0
			for n < len(entries) && entries[n].Seq <= through {
				n++
			}
			if n == 0 {
				break
			}
			letters := make([]spool.DeadLetter, n)
			for i, e := range entries[:n] {
				letters[i] = spool.DeadLetter{Event: e.Event, Reason: fmt.Sprintf("tenant %s could not be set up at %s within %s of this event being parked (%s): %s",
					g.tenant, name, mh, e.At.UTC().Format(time.RFC3339), b.err)}
			}
			if _, err := d.spool.AddDeadLetters(name, letters); err != nil {
				return err
			}
			last := entries[n-1]
			if err := d.opts.Held.Release(name, key, last.Seq, n, last.end); err != nil {
				return err
			}
			// Never to be sent from the ledger again either (a retried batch).
			d.shipped.set(name, key, last.Seq)
			total += n
		}
	}
	if total > 0 {
		log.Printf("[pipeline] WARNING: %s: dead-lettered %d event(s) of tenant %s: they were parked for %s and the tenant still cannot be set up (%s). "+
			"They stay replayable: fix the cause, then POST /api/v1/destinations/%s/dead-letters/replay", name, total, g.tenant, mh, b.err, name)
		d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", map[string]string{"destination": name}, float64(total))
		d.update(name, func(s *Status) { s.DeadLettered += uint64(total) })
		d.publishHeld(name)
		d.reportBlock(name, *b)
		d.publishBlocked(name)
	}
	return ctx.Err()
}

// ReplayResult is what became of dead letters sent again.
type ReplayResult struct {
	// Refused maps the events the destination still refuses to its reason.
	Refused map[string]string
	// NotSetUp maps the tenants the destination could not set up to its
	// error. Their events were not sent: they would have nowhere to go.
	NotSetUp map[string]string
}

// Replay sends dead letters to a destination again, as delivery would: the
// tenants are set up first, the events of a tenant that cannot be set up are
// kept back, and a batch the destination refuses as a whole is bisected, so
// one event it still refuses never keeps the others from being delivered.
// An error means nothing is known to be delivered.
func (d *Dispatcher) Replay(ctx context.Context, name string, events []usage.Event) (ReplayResult, error) {
	res := ReplayResult{Refused: map[string]string{}, NotSetUp: map[string]string{}}
	dest := d.destination(name)
	if dest == nil {
		return res, d.notConfigured(name)
	}
	if state := stateOf(d.spool.DestinationState(name)); state != StateDelivering {
		return res, &StateError{Destination: name, State: state,
			Msg: "nothing is sent to it, dead letters included, until it delivers again"}
	}
	send := events
	if d.opts.Tenants != nil {
		first, order := firstWindows(len(events), func(i int) *usage.Event { return &events[i] })
		for _, id := range order {
			if d.tenantReady(name, id) {
				continue
			}
			b, interrupted := d.provision(ctx, dest, id, first[id], true, true)
			if interrupted {
				return res, ctx.Err()
			}
			if b != nil && !b.permanent {
				res.NotSetUp[id] = b.err
			}
		}
		if len(res.NotSetUp) > 0 {
			send = nil
			for _, ev := range events {
				if _, kept := res.NotSetUp[ev.Tenant]; !kept {
					send = append(send, ev)
				}
			}
		}
	}
	refused, err := d.send(ctx, dest, send)
	if err != nil {
		return res, err
	}
	if len(refused) < len(send) {
		d.noteAccepted(name)
	}
	for id, reason := range refused {
		res.Refused[id] = reason
	}
	d.kick(name) // a tenant set up here has parked events to ship
	return res, nil
}
