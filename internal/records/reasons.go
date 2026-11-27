package records

import "strings"

// Reason is why an event or a record was refused, one of a small fixed
// set. It is the reason of a Rejection and the reason label of
// vbilling_records_rejected_total and vbilling_ingest_events_rejected_total.
type Reason string

const (
	// ReasonTenant: no tenant, a tenant vBilling does not know (with
	// RECORDS_UNKNOWN_TENANTS=reject), or not the tenant the token is bound to.
	ReasonTenant Reason = "tenant"
	// ReasonReservedMetric: a metric vBilling measures itself, or one that
	// only the other push path writes (safeguard.PushedMetric).
	ReasonReservedMetric Reason = "reserved_metric"
	// ReasonCluster: a cluster vBilling does not know for the tenant, or one
	// that did not exist in the usage's window (safeguard.Clusters).
	ReasonCluster Reason = "cluster"
	// ReasonUnknownMetric: a metric that is not declared.
	ReasonUnknownMetric Reason = "unknown_metric"
	// ReasonTooOld: older than the oldest usage vBilling, or one of its
	// destinations, accepts.
	ReasonTooOld Reason = "too_old"
	// ReasonInFuture: ahead of vBilling's clock.
	ReasonInFuture Reason = "in_future"
	// ReasonDuplicate: an event without an id that repeats another event of
	// its request.
	ReasonDuplicate Reason = "duplicate"
	// ReasonInvalid: anything malformed or unsafe: IDs, values, quantities,
	// dimensions, properties, windows.
	ReasonInvalid Reason = "invalid"
)

// Reasons lists every Reason, the most telling first: an event or record
// with several problems counts under the first of them here.
var Reasons = []Reason{ReasonTenant, ReasonReservedMetric, ReasonCluster, ReasonUnknownMetric, ReasonTooOld, ReasonInFuture, ReasonDuplicate, ReasonInvalid}

// UnacceptedClient is the client label of the counters of events and
// records sent under a client name (X-VBilling-Client) that never had a
// push accepted. Whoever holds an ingest token chooses the name, so a name
// labels a series only once a push under it stored usage: refused requests
// can never add series. It is not a valid client name.
const UnacceptedClient = "_unaccepted"

// Problems collects why one event or record is refused: every problem, and
// the Reason it counts under.
type Problems struct {
	texts  []string
	reason Reason
}

// Add notes a problem.
func (p *Problems) Add(reason Reason, text string) {
	p.texts = append(p.texts, text)
	if p.reason == "" || rank(reason) < rank(p.reason) {
		p.reason = reason
	}
}

// Any reports whether a problem was noted.
func (p *Problems) Any() bool { return len(p.texts) > 0 }

// Reason is the reason the event or record counts under.
func (p *Problems) Reason() Reason { return p.reason }

// String lists the problems.
func (p *Problems) String() string { return strings.Join(p.texts, "; ") }

func rank(r Reason) int {
	for i, x := range Reasons {
		if x == r {
			return i
		}
	}
	return len(Reasons)
}
