package stripe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// UseLedger allows the upgrade to retain existing meters and prices when
// retained evidence identifies a single original split identity.
func (a *Adapter) UseLedger(s *spool.Spool) { a.ledger = s }

func (a *Adapter) meterIdentity(e *usage.Event) (string, bool) {
	var b strings.Builder
	b.WriteString(e.Metric)
	split := false
	for _, key := range a.cfg.StripeSplitMetersBy {
		value := fieldValue(e, key)
		b.WriteByte(0)
		b.WriteString(key)
		b.WriteByte(0)
		b.WriteString(value)
		split = split || value != ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16]), split
}

func (a *Adapter) legacyMeterName(e *usage.Event) string {
	name := e.Metric
	for _, key := range a.cfg.StripeSplitMetersBy {
		if value := fieldValue(e, key); value != "" {
			name += "__" + slug(value)
		}
	}
	if len(name) > 100 {
		sum := sha256.Sum256([]byte(name))
		name = name[:91] + "_" + hex.EncodeToString(sum[:4])
	}
	return name
}

func (a *Adapter) prepareMeterRoutes(ctx context.Context) error {
	a.routeInitMu.Lock()
	defer a.routeInitMu.Unlock()
	if a.routesReady {
		return nil
	}
	routes := a.state.WithPrefix("meter-route/")
	if _, migrated := a.state.Get("meter-routes-v1"); !migrated {
		a.meterMu.Lock()
		err := a.loadMetersLocked(ctx)
		names := make(map[string]bool, len(a.meters))
		for name := range a.meters {
			names[name] = true
		}
		a.meterMu.Unlock()
		if err != nil {
			return err
		}
		// A slug alone cannot identify a meter's original SKU. Bind an
		// existing name only if retained pre-upgrade usage has one identity.
		// History already removed by retention cannot be reconstructed here.
		identities := map[string]map[string]bool{}
		if a.ledger != nil {
			err = a.ledger.ScanAll(time.Time{}, a.now().Add(24*time.Hour), func(e *usage.Event) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				name := a.legacyMeterName(e)
				id, split := a.meterIdentity(e)
				if names[name] && split {
					if identities[name] == nil {
						identities[name] = map[string]bool{}
					}
					identities[name][id] = true
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("identify existing Stripe meters: %w", err)
			}
		}
		for name, ids := range identities {
			if len(ids) != 1 {
				log.Printf("[stripe] legacy meter %s represents %d distinct split identities; future usage uses separate meters that need prices", name, len(ids))
				continue
			}
			for id := range ids {
				if err := a.state.Set("meter-route/"+id, name); err != nil {
					return err
				}
				routes[id] = name
			}
		}
		if err := a.state.Set("meter-routes-v1", "true"); err != nil {
			return err
		}
	}
	a.routeMu.Lock()
	a.routes = routes
	a.routeMu.Unlock()
	a.routesReady = true
	return nil
}
