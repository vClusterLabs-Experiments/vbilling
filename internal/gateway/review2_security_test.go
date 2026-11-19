package gateway

// Second independent review (v0.3.0). Every TestReview2* test here fails on
// v0.3.0 and describes the expected behavior.

import (
	"context"
	"errors"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// review2Blocker is LiteLLM restarting: a block call is refused before it
// is sent (connection refused), so it certainly did not happen.
type review2Blocker struct {
	blocked bool
	down    bool
}

func (*review2Blocker) Name() string                                      { return "litellm" }
func (b *review2Blocker) IsBlocked(context.Context, string) (bool, error) { return b.blocked, nil }
func (b *review2Blocker) Block(context.Context, string) error {
	if b.down {
		return errors.New(`Post "http://litellm:4000/team/block": dial tcp 10.0.0.9:4000: connect: connection refused`)
	}
	b.blocked = true
	return nil
}
func (b *review2Blocker) Unblock(context.Context, string) error { b.blocked = false; return nil }

// PR #9 claims an identity before the block call, so a block whose reply
// is lost stays vBilling's. The claim is kept even when the call certainly
// failed. If an administrator then blocks the team by hand (abuse), the
// next resync "re-blocks" it as vBilling's, and the tenant's payment lifts
// the administrator's block. Before PR #9 the existing block was left alone.
func TestReview2DefinitelyFailedBlockMustNotClaimAnAdministratorsBlock(t *testing.T) {
	ctx := context.Background()
	b := &review2Blocker{down: true}
	g := New(kv.Memory(), nil, func(string) []string { return []string{"team-acme"} }, b)
	if err := g.Apply(ctx, "acme", true); err == nil {
		t.Fatal("expected the refused block call")
	}
	b.down = false
	b.blocked = true                                   // an administrator blocks the team by hand (abuse)
	if err := g.Apply(ctx, "acme", true); err != nil { // resync while acme is still suspended
		t.Fatal(err)
	}
	if err := g.Apply(ctx, "acme", false); err != nil { // acme pays
		t.Fatal(err)
	}
	if !b.blocked {
		t.Fatalf("the tenant's payment lifted an administrator's block: vBilling's only block call was refused, yet it claimed the team (%v)", g.Blocked())
	}
}
