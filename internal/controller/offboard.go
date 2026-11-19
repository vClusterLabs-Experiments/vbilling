package controller

import (
	"fmt"
	"log"

	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
)

// heldUsage is the store of events held back from destinations until they
// are priced (PRICE_CHECK=hold).
type heldUsage interface {
	Each(dest string, fn func(pipeline.HeldEntry) bool) error
}

// UseHeld makes offboarding wait for a tenant's held usage too.
func (c *Controller) UseHeld(h heldUsage) { c.held = h }

// offboardScanLimit bounds the ledger records one reconcile reads per tenant
// looking for its last event; the next reconcile continues.
const offboardScanLimit = 100_000

// offboarding finds each departed tenant's last ledger event. Offboarding
// ends billing in a destination (Lago terminates the subscription and
// issues the final invoice, and then ignores events of that subscription),
// so it waits until the destination has the tenant's usage.
type offboarding struct {
	seen    map[string]uint64 // tenant -> ledger head when it last had a cluster
	scanned map[string]uint64 // tenant -> ledger read through this seq since
	last    map[string]uint64 // tenant -> its last event found (at least seen)
	logged  map[string]string // tenant/destination -> wait reason last logged
}

func (o *offboarding) init() {
	if o.seen == nil {
		o.seen, o.scanned, o.last, o.logged = map[string]uint64{}, map[string]uint64{}, map[string]uint64{}, map[string]string{}
	}
}

// noteSeen records the ledger head for tenants that have clusters. c.mu held.
func (o *offboarding) noteSeen(id string, head uint64) {
	o.init()
	o.seen[id] = head
	delete(o.scanned, id)
	delete(o.last, id)
}

// forget drops a tenant offboarded everywhere. c.mu held.
func (o *offboarding) forget(id string) {
	o.init()
	delete(o.seen, id)
	delete(o.scanned, id)
	delete(o.last, id)
	for k := range o.logged {
		if len(k) > len(id) && k[:len(id)+1] == id+"/" {
			delete(o.logged, k)
		}
	}
}

// lastEvent returns a ledger sequence at or after the tenant's last event,
// and whether it is final: false while the ledger after the tenant was last
// seen has not been read through yet (the next reconcile continues).
// Every event committed before the tenant was last seen lies at or below
// the ledger head of that moment; later ones are found by reading on.
func (c *Controller) lastEvent(tenant string) (uint64, bool, error) {
	c.mu.Lock()
	c.off.init()
	from, ok := c.off.scanned[tenant]
	if !ok {
		from = c.off.seen[tenant]
		c.off.last[tenant] = from
	}
	last := c.off.last[tenant]
	c.mu.Unlock()
	read, complete := 0, false
	for read < offboardScanLimit {
		recs, err := c.spool.Read(from, 5000)
		if err != nil {
			return 0, false, err
		}
		if len(recs) == 0 {
			complete = true
			break
		}
		for _, r := range recs {
			if r.Event != nil && r.Event.Tenant == tenant {
				last = r.Seq
			}
		}
		from = recs[len(recs)-1].Seq
		read += len(recs)
	}
	c.mu.Lock()
	c.off.scanned[tenant], c.off.last[tenant] = from, last
	c.mu.Unlock()
	return last, complete, nil
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
			return false, "some of its usage is held until priced (PRICE_CHECK=hold)"
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
