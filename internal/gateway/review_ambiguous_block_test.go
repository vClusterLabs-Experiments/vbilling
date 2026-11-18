package gateway

import (
	"context"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// The upstream commits the block, but its reply never reaches the caller.
type reviewLostBlockReply struct {
	blocked bool
	lost    bool
}

func (*reviewLostBlockReply) Name() string { return "litellm" }
func (b *reviewLostBlockReply) IsBlocked(context.Context, string) (bool, error) {
	return b.blocked, nil
}
func (b *reviewLostBlockReply) Block(context.Context, string) error {
	b.blocked = true
	if !b.lost {
		b.lost = true
		return context.DeadlineExceeded
	}
	return nil
}
func (b *reviewLostBlockReply) Unblock(context.Context, string) error {
	b.blocked = false
	return nil
}

func TestReviewLostBlockReplyMustNotStrandPayingTenant(t *testing.T) {
	ctx := context.Background()
	b := &reviewLostBlockReply{}
	state := kv.Memory()
	identities := func(string) []string { return []string{"acme-team"} }
	g := New(state, nil, identities, b)
	if err := g.Apply(ctx, "acme", true); err == nil {
		t.Fatal("expected the lost block reply")
	}
	// Resync (also after a restart) must remember that the existing block
	// may be its own rather than silently treating it as an operator's.
	g = New(state, nil, identities, b)
	if err := g.Apply(ctx, "acme", true); err != nil {
		t.Fatal(err)
	}
	if err := g.Apply(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
	if b.blocked {
		t.Fatalf("a paid tenant remains permanently blocked after one lost response; ownership=%v", g.Blocked())
	}
}
