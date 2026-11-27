package controller

import (
	"fmt"
	"log"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// heldUsage is the store of events held back from destinations until they
// are priced (PRICE_CHECK=hold).
type heldUsage interface {
	Each(dest string, fn func(pipeline.HeldEntry) bool) error
}

// UseHeld makes offboarding wait for a tenant's held usage too.
func (c *Controller) UseHeld(h heldUsage) { c.held = h }

// usageReadBatch is how many ledger records usageSince reads at a time.
const usageReadBatch = 5000

// usageSince reads the ledger committed since the last reconcile and
// returns the tenants with usage in it, of any source (records, ingest,
// Prometheus, the collector), each with when its latest usage was appended
// to the ledger. That is what keeps a tenant whose usage arrives as hourly
// totals: their windows end an hour before they arrive. A tenant that lost
// its last cluster but still has usage is not offboarded (its later usage
// would be ignored: Lago terminates its subscription).
//
// Each reconcile reads on from where the previous one stopped (a cursor
// kept between reconciles) through head, the ledger's end when it began, a
// batch at a time, however much was committed meanwhile: with a limit per
// reconcile, a ledger that grows faster than the limit would never be read
// through, and nobody would be offboarded again. The same read finds each
// tenant's last event (lastEvent). readThrough is false only when the
// ledger could not be read; the next reconcile goes on. The first call
// starts at head.
func (c *Controller) usageSince(head uint64) (tenants map[string]time.Time, readThrough bool) {
	if c.spool == nil {
		return nil, true
	}
	c.mu.Lock()
	from, started := c.usageRead, c.usageStarted
	if !started {
		c.usageRead, c.usageStarted, c.usageThrough = head, true, true
	}
	c.mu.Unlock()
	if !started {
		return nil, true
	}
	tenants = map[string]time.Time{}
	last := map[string]uint64{}
	var err error
	for from < head {
		var recs []spool.Record
		if recs, err = c.spool.Read(from, usageReadBatch); err != nil {
			log.Printf("[controller] read the ledger for recent usage: %v (nobody is offboarded until it can)", err)
			break
		}
		if len(recs) == 0 {
			from = head // nothing more to read (compacted, or the head moved back)
			break
		}
		for _, r := range recs {
			if e := r.Event; e != nil && e.Tenant != "" {
				if r.At.After(tenants[e.Tenant]) {
					tenants[e.Tenant] = r.At
				}
				last[e.Tenant] = r.Seq
			}
		}
		from = recs[len(recs)-1].Seq
	}
	readThrough = err == nil && from >= head
	c.mu.Lock()
	c.usageRead, c.usageThrough = from, readThrough
	c.off.init()
	for t, seq := range last {
		if seq > c.off.lastSeq[t] {
			c.off.lastSeq[t] = seq
		}
	}
	c.mu.Unlock()
	return tenants, readThrough
}

// offboarding finds each departed tenant's last ledger event. Offboarding
// ends billing in a destination (Lago terminates the subscription and
// issues the final invoice, and then ignores events of that subscription),
// so it waits until the destination has the tenant's usage.
type offboarding struct {
	seen    map[string]uint64 // tenant -> ledger head when it last had a cluster
	lastSeq map[string]uint64 // tenant -> its last event usageSince read
	logged  map[string]string // tenant/destination -> wait reason last logged
}

func (o *offboarding) init() {
	if o.seen == nil {
		o.seen, o.lastSeq, o.logged = map[string]uint64{}, map[string]uint64{}, map[string]string{}
	}
}

// noteSeen records the ledger head for tenants that have clusters. c.mu held.
func (o *offboarding) noteSeen(id string, head uint64) {
	o.init()
	o.seen[id] = head
}

// forget drops a tenant offboarded everywhere. c.mu held.
func (o *offboarding) forget(id string) {
	o.init()
	delete(o.seen, id)
	delete(o.lastSeq, id)
	for k := range o.logged {
		if len(k) > len(id) && k[:len(id)+1] == id+"/" {
			delete(o.logged, k)
		}
	}
}

// lastEvent returns a ledger sequence at or after the tenant's last event,
// and whether it is final: false while the ledger has not been read through
// (usageSince; the next reconcile continues). Every event committed before
// the tenant was last seen with a cluster lies at or below the ledger head
// of that moment; later ones are the ones usageSince read.
func (c *Controller) lastEvent(tenant string) (uint64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off.init()
	return max(c.off.seen[tenant], c.off.lastSeq[tenant]), c.usageThrough, nil
}

// delivered reports whether dest has everything of the tenant: its cursor
// is past the tenant's last ledger event, and none of the tenant's events
// is held from it. Otherwise it says why not.
func (c *Controller) delivered(tenant, dest string) (bool, string) {
	if c.spool == nil {
		return true, ""
	}
	last, complete, err := c.lastEvent(tenant)
	switch {
	case err != nil:
		return false, "reading the ledger: " + err.Error()
	case !complete:
		return false, "still reading the ledger for its last event"
	}
	if cur := c.spool.Cursor(dest); cur < last {
		return false, fmt.Sprintf("its usage through ledger seq %d is not delivered yet (cursor %d)", last, cur)
	}
	if c.held != nil {
		held := false
		err := c.held.Each(dest, func(e pipeline.HeldEntry) bool {
			held = e.Event.Tenant == tenant
			return !held
		})
		if err != nil {
			return false, "reading held events: " + err.Error()
		}
		if held {
			return false, "some of its usage is still waiting in vBilling: held at the pricing gate (no price yet, or not counted by the billing platform yet), or parked because the tenant could not be set up at the billing platform"
		}
	}
	return true, ""
}

// logWaiting logs, once per reason, that offboarding a tenant waits.
func (c *Controller) logWaiting(tenant, dest, why string) {
	key := tenant + "/" + dest
	c.mu.Lock()
	c.off.init()
	same := c.off.logged[key] == why
	c.off.logged[key] = why
	c.mu.Unlock()
	if !same {
		log.Printf("[controller] offboarding %s from %s waits: %s", tenant, dest, why)
	}
}
