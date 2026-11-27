package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Stripe guarantees identifier deduplication for at least 24 hours. Leave
// an hour for clocks and retries. Older ambiguous outcomes require review.
const safeRetryWindow = 23 * time.Hour
const receiptsKey = "delivery-receipts/v1"

type receiptState struct {
	mu        sync.Mutex
	loaded    bool
	enabledAt time.Time
	entries   map[string]destinations.DeliveryReceipt
}

func (a *Adapter) loadReceiptsLocked() error {
	if a.receipts.loaded {
		return nil
	}
	enabledAt := a.now().UTC()
	if raw, ok := a.state.Get("delivery-receipts-enabled-at"); ok {
		var err error
		enabledAt, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return fmt.Errorf("read Stripe delivery receipt epoch: %w", err)
		}
	} else if err := a.state.Set("delivery-receipts-enabled-at", enabledAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	entries := map[string]destinations.DeliveryReceipt{}
	if raw, ok := a.state.Get(receiptsKey); ok {
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return fmt.Errorf("read Stripe delivery receipts: %w", err)
		}
		if entries == nil {
			return fmt.Errorf("Stripe delivery receipts are null")
		}
		for id, r := range entries {
			if r.Status != "pending" && r.Status != "delivered" && r.Status != "not_delivered" {
				return fmt.Errorf("Stripe delivery receipt %s has invalid status %q", id, r.Status)
			}
			if r.Status == "pending" && r.FirstAttempt.IsZero() {
				return fmt.Errorf("Stripe delivery receipt %s has no attempt time", id)
			}
		}
	}
	a.receipts.entries, a.receipts.enabledAt, a.receipts.loaded = entries, enabledAt, true
	return nil
}

func (a *Adapter) saveReceiptsLocked(entries map[string]destinations.DeliveryReceipt) error {
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	raw := string(b)
	err = a.state.Set(receiptsKey, raw)
	// kv keeps a renamed file in memory even when directory fsync fails.
	// Keep the same conservative state here, and still report the error.
	if stored, ok := a.state.Get(receiptsKey); ok && stored == raw {
		a.receipts.entries = entries
	}
	return err
}

func (a *Adapter) copyReceiptsLocked() map[string]destinations.DeliveryReceipt {
	out := make(map[string]destinations.DeliveryReceipt, len(a.receipts.entries))
	for id, r := range a.receipts.entries {
		out[id] = r
	}
	return out
}

// prepareAttempts durably records every possible network attempt before
// sending. A crash leaves either a confirmed receipt or a bounded retry.
func (a *Adapter) prepareAttempts(jobs []job) (skip map[string]bool, blocked map[string]string, err error) {
	a.receipts.mu.Lock()
	defer a.receipts.mu.Unlock()
	if err := a.loadReceiptsLocked(); err != nil {
		return nil, nil, err
	}
	now := a.now().UTC()
	skip, blocked = map[string]bool{}, map[string]string{}
	next := a.copyReceiptsLocked()
	changed := false
	for _, j := range jobs {
		r, ok := next[j.ev.ID]
		if !ok && !j.ev.RecordedAt.IsZero() && j.ev.RecordedAt.Before(a.receipts.enabledAt) && now.Sub(j.ev.RecordedAt) >= safeRetryWindow {
			// An unacknowledged pre-upgrade batch might already have been
			// accepted by Stripe. Its old adapter left no durable receipt.
			r = destinations.DeliveryReceipt{Status: "pending", FirstAttempt: j.ev.RecordedAt, UpdatedAt: now}
			next[j.ev.ID], changed, ok = r, true, true
		}
		if ok && r.Status == "delivered" {
			skip[j.ev.ID] = true
			continue
		}
		if ok && r.Status == "pending" && now.Sub(r.FirstAttempt) >= safeRetryWindow {
			blocked[j.ev.ID] = fmt.Sprintf("Stripe delivery outcome ambiguous since %s: its guaranteed identifier deduplication window has elapsed; reconcile and resolve this receipt before replay", r.FirstAttempt.Format(time.RFC3339))
			continue
		}
		if !ok || r.Status == "not_delivered" {
			r.Status, r.FirstAttempt, r.UpdatedAt = "pending", now, now
			next[j.ev.ID], changed = r, true
		}
	}
	if changed {
		if err := a.saveReceiptsLocked(next); err != nil {
			return nil, nil, fmt.Errorf("persist Stripe delivery attempts: %w", err)
		}
	}
	return skip, blocked, nil
}

func (a *Adapter) confirmDelivery(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	a.receipts.mu.Lock()
	defer a.receipts.mu.Unlock()
	if err := a.loadReceiptsLocked(); err != nil {
		return err
	}
	next := a.copyReceiptsLocked()
	for _, id := range ids {
		r := next[id]
		r.Status, r.UpdatedAt = "delivered", a.now().UTC()
		next[id] = r
	}
	return a.saveReceiptsLocked(next)
}

func (a *Adapter) CheckpointDelivery(events []usage.Event) error {
	a.receipts.mu.Lock()
	defer a.receipts.mu.Unlock()
	if err := a.loadReceiptsLocked(); err != nil {
		return err
	}
	next := a.copyReceiptsLocked()
	changed := false
	for _, e := range events {
		if r, ok := next[e.ID]; ok && r.Status == "delivered" {
			delete(next, e.ID)
			changed = true
		}
	}
	if changed {
		return a.saveReceiptsLocked(next)
	}
	return nil
}

func (a *Adapter) DeliveryReceipt(id string) (destinations.DeliveryReceipt, bool, error) {
	a.receipts.mu.Lock()
	defer a.receipts.mu.Unlock()
	if err := a.loadReceiptsLocked(); err != nil {
		return destinations.DeliveryReceipt{}, false, err
	}
	r, ok := a.receipts.entries[id]
	return r, ok, nil
}

func (a *Adapter) ResolveDelivery(id, outcome, reason string) error {
	if outcome != "delivered" && outcome != "not_delivered" {
		return fmt.Errorf("outcome must be delivered or not_delivered")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 1024 {
		return fmt.Errorf("reason must be 1-1024 bytes and describe the reconciliation evidence")
	}
	a.receipts.mu.Lock()
	defer a.receipts.mu.Unlock()
	if err := a.loadReceiptsLocked(); err != nil {
		return err
	}
	r, ok := a.receipts.entries[id]
	if !ok || r.Status == "delivered" {
		return fmt.Errorf("no unresolved delivery receipt for %s", id)
	}
	r.Status, r.Reason, r.UpdatedAt = outcome, reason, a.now().UTC()
	next := a.copyReceiptsLocked()
	next[id] = r
	return a.saveReceiptsLocked(next)
}

func (a *Adapter) ValidateReplay(ctx context.Context, from, to uint64) error {
	if to >= from {
		return nil
	}
	if a.ledger == nil {
		return fmt.Errorf("Stripe replay requires the usage ledger to validate its deduplication window")
	}
	cutoff := a.now().Add(-safeRetryWindow)
	for cursor := to; cursor < from; {
		if err := ctx.Err(); err != nil {
			return err
		}
		recs, err := a.ledger.Read(cursor, 500)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			if r.Seq > from {
				return nil
			}
			if e := r.Event; e != nil && destinations.AcceptsEvent(a, e) && e.WindowStart.Before(cutoff) {
				return fmt.Errorf("Stripe replay includes usage older than its guaranteed deduplication window (%s); reconcile before recovering old usage", e.ID)
			}
			cursor = r.Seq
		}
	}
	return nil
}
