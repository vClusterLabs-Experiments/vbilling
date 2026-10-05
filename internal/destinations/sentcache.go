package destinations

import "sync"

// SentCache remembers recently delivered event IDs so a retried batch only
// resends what actually failed. It is bounded; the oldest IDs fall out first.
type SentCache struct {
	mu   sync.Mutex
	set  map[string]struct{}
	ring []string
	next int
}

// NewSentCache keeps up to n IDs.
func NewSentCache(n int) *SentCache {
	if n <= 0 {
		n = 1
	}
	return &SentCache{set: make(map[string]struct{}, n), ring: make([]string, n)}
}

func (c *SentCache) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.set[id]
	return ok
}

func (c *SentCache) Add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.set[id]; ok {
		return
	}
	if old := c.ring[c.next]; old != "" {
		delete(c.set, old)
	}
	c.ring[c.next] = id
	c.set[id] = struct{}{}
	c.next = (c.next + 1) % len(c.ring)
}
