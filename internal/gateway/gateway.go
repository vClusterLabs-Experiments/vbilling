// Package gateway stops a tenant's requests at its AI gateway while the
// tenant is suspended or paused.
//
// A tenant that buys tokens has no cluster to stop: its requests reach a
// shared model through a gateway, so the gateway is where a suspension for
// non-payment takes effect. A paused cluster needs it too: a gateway that
// retries requests while the cluster's model server is stopping bills
// requests no client ever received (seen live with LiteLLM).
//
// vBilling blocks the tenant's gateway identities (its tenant ID and its
// aliases, for LiteLLM the team IDs) and later unblocks only the identities
// it blocked, never one an administrator blocked by hand.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// ErrUnknown is returned for an identity the gateway does not know, such
// as a tenant ID that is not one of its teams. It is not an error to block.
var ErrUnknown = errors.New("not known to the gateway")

// Blocker blocks and unblocks one kind of gateway identity.
type Blocker interface {
	Name() string
	// IsBlocked reports whether the identity is blocked now, by anyone.
	IsBlocked(ctx context.Context, identity string) (bool, error)
	Block(ctx context.Context, identity string) error
	Unblock(ctx context.Context, identity string) error
}

// Gate applies tenants' states to their gateway identities.
type Gate struct {
	blockers   []Blocker
	identities func(tenant string) []string
	state      *kv.Store // "blocked/<blocker>/<identity>" -> tenant
	tel        *telemetry.Registry
	mu         sync.Mutex
}

// New returns a gate. identities lists a tenant's gateway identities; state
// remembers what the gate blocked, so it survives restarts.
func New(state *kv.Store, tel *telemetry.Registry, identities func(tenant string) []string, blockers ...Blocker) *Gate {
	if tel == nil {
		tel = telemetry.New()
	}
	return &Gate{blockers: blockers, identities: identities, state: state, tel: tel}
}

func key(blocker, identity string) string { return "blocked/" + blocker + "/" + identity }

// Apply blocks the tenant's identities when blocked is true, and otherwise
// unblocks the ones this gate blocked. It is idempotent, so it can run on
// every resync and retry what failed.
func (g *Gate) Apply(ctx context.Context, tenant string, blocked bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var errs []error
	for _, b := range g.blockers {
		if blocked {
			errs = append(errs, g.block(ctx, b, tenant))
		} else {
			errs = append(errs, g.unblock(ctx, b, tenant))
		}
	}
	return errors.Join(errs...)
}

func (g *Gate) block(ctx context.Context, b Blocker, tenant string) error {
	var errs []error
	for _, id := range unique(g.identities(tenant)) {
		k := key(b.Name(), id)
		_, ours := g.state.Get(k)
		if !ours {
			// Blocked already, by someone else: leave it to them, so the
			// tenant's reactivation never lifts an administrator's block.
			already, err := b.IsBlocked(ctx, id)
			if errors.Is(err, ErrUnknown) {
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: read %s for tenant %s: %w", b.Name(), id, tenant, err))
				continue
			}
			if already {
				continue
			}
			// Not blocked: claim it before the call, so a block whose reply
			// is lost is still ours to retry and to lift on reactivation.
			if err := g.state.Set(k, tenant); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		err := b.Block(ctx, id)
		if errors.Is(err, ErrUnknown) {
			if derr := g.state.Delete(k); derr != nil {
				errs = append(errs, derr)
			}
			continue
		}
		if err != nil {
			if !ours && Definite(err) {
				// The block certainly did not happen: drop the claim this
				// call made. Kept, an administrator's block made before the
				// next try would look like vBilling's, and a payment would
				// lift it.
				if derr := g.state.Delete(k); derr != nil {
					errs = append(errs, derr)
				}
			}
			errs = append(errs, fmt.Errorf("%s: block %s for tenant %s: %w", b.Name(), id, tenant, err))
			g.tel.Add("vbilling_gateway_block_errors_total", "Failed gateway block or unblock calls.", map[string]string{"gateway": b.Name()}, 1)
			continue
		}
		if !ours {
			log.Printf("[gateway] %s: blocked %s (tenant %s)", b.Name(), id, tenant)
		}
	}
	return errors.Join(errs...)
}

func (g *Gate) unblock(ctx context.Context, b Blocker, tenant string) error {
	var errs []error
	prefix := key(b.Name(), "")
	held := g.state.WithPrefix(prefix)
	ids := make([]string, 0, len(held))
	for k, t := range held {
		if t == tenant {
			ids = append(ids, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := b.Unblock(ctx, id); err != nil && !errors.Is(err, ErrUnknown) {
			errs = append(errs, fmt.Errorf("%s: unblock %s for tenant %s: %w", b.Name(), id, tenant, err))
			g.tel.Add("vbilling_gateway_block_errors_total", "Failed gateway block or unblock calls.", map[string]string{"gateway": b.Name()}, 1)
			continue
		}
		log.Printf("[gateway] %s: unblocked %s (tenant %s)", b.Name(), id, tenant)
		if err := g.state.Delete(key(b.Name(), id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Blocked lists what the gate has blocked: blocker -> identity -> tenant.
func (g *Gate) Blocked() map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, t := range g.state.WithPrefix("blocked/") {
		parts := strings.SplitN(strings.TrimPrefix(k, "blocked/"), "/", 2)
		if len(parts) != 2 {
			continue
		}
		if out[parts[0]] == nil {
			out[parts[0]] = map[string]string{}
		}
		out[parts[0]][parts[1]] = t
	}
	return out
}

// StatusError is a gateway's error answer to a call.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body) }

// Definite reports whether a failed call certainly changed nothing: the
// gateway answered with an error, or the request was never sent
// (connection refused, unknown host). A timeout, a connection lost once
// the request was out, or a proxy's 502 or 504 may hide a call that took
// effect.
func Definite(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code != http.StatusBadGateway && status.Code != http.StatusGatewayTimeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	// A blocker that reports such errors only as text.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host")
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// LiteLLM blocks LiteLLM teams (POST /team/block and /team/unblock with an
// admin key). A blocked team's keys are refused before any model is called.
type LiteLLM struct {
	URL string
	Key string
	HC  *http.Client
}

// NewLiteLLM returns a LiteLLM team blocker.
func NewLiteLLM(url, key string) *LiteLLM {
	return &LiteLLM{URL: strings.TrimRight(url, "/"), Key: key, HC: &http.Client{Timeout: 15 * time.Second}}
}

func (l *LiteLLM) Name() string { return "litellm" }

// IsBlocked reads GET /team/info (team_info.blocked).
func (l *LiteLLM) IsBlocked(ctx context.Context, team string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL+"/team/info?team_id="+url.QueryEscape(team), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+l.Key)
	resp, err := l.HC.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		if unknownTeam(resp.StatusCode, msg) {
			return false, ErrUnknown
		}
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg[:min(len(msg), 2048)])))
	}
	var info struct {
		TeamInfo struct {
			Blocked bool `json:"blocked"`
		} `json:"team_info"`
	}
	if err := json.Unmarshal(msg, &info); err != nil {
		return false, fmt.Errorf("team info: %w", err)
	}
	return info.TeamInfo.Blocked, nil
}

func (l *LiteLLM) Block(ctx context.Context, team string) error {
	return l.post(ctx, "/team/block", team)
}

func (l *LiteLLM) Unblock(ctx context.Context, team string) error {
	return l.post(ctx, "/team/unblock", team)
}

func (l *LiteLLM) post(ctx context.Context, path, team string) error {
	body, _ := json.Marshal(map[string]string{"team_id": team})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.Key)
	resp, err := l.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	if unknownTeam(resp.StatusCode, msg) {
		return ErrUnknown
	}
	return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
}

// unknownTeam recognizes LiteLLM's answer for a team that does not exist.
func unknownTeam(status int, body []byte) bool {
	if status != http.StatusNotFound && status != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(string(body))
	return strings.Contains(b, "not found") || strings.Contains(b, "does not exist") || strings.Contains(b, "doesn't exist")
}
