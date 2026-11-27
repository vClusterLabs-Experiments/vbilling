package destinations

import "time"

// AgeLimiter is implemented by destinations that refuse usage older than a
// fixed age: a billing platform that only takes events backdated so far.
// The ingest API refuses pushed usage older than the tightest limit among
// the configured destinations, so no emitter is told its usage is stored
// when a destination is certain to dead-letter it.
type AgeLimiter interface {
	// OldestUsage is the oldest usage the destination accepts: how long
	// before now an event's window may start. It is the limit the
	// destination's own SendEvents enforces.
	OldestUsage() time.Duration
}

// OldestUsage returns the tightest age limit the destinations declare and
// the name of the destination that sets it. ok is false when none declares
// one.
func OldestUsage(dests []Destination) (limit time.Duration, by string, ok bool) {
	for _, d := range dests {
		l, declares := d.(AgeLimiter)
		if !declares {
			continue
		}
		if age := l.OldestUsage(); age > 0 && (!ok || age < limit) {
			limit, by, ok = age, d.Name(), true
		}
	}
	return limit, by, ok
}
