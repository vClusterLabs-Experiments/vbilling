package enforcement

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// What an operator can see of enforcement without reading the log: what
// became of each billing webhook, a payment that did not unlock its tenant,
// a cluster whose API refuses vBilling, a suspension someone took apart
// inside a cluster, and every tenant's billing state.

// UseHealth reports, as sources (GET /api/v1/sources, vbilling_sources):
// billing-unlock/<tenant> while a payment has not lifted the tenant's
// billing hold, and enforcement-access/<cluster> while the cluster's API
// refuses what vBilling needs to suspend or pause inside it. Call it before
// the enforcer is used.
func (e *Enforcer) UseHealth(h *health.Registry) { e.opts.Health = h }

// UseTenants names the tenants vBilling knows from discovery, so that
// vbilling_tenant_billing_state has a series for each of them (active when
// it has no hold) and loses those of tenants that were offboarded. Call it
// before the enforcer is used.
func (e *Enforcer) UseTenants(list func() []string) { e.opts.Tenants = list }

// UseClusters lists every cluster metering has (ok is false until the
// first discovery succeeded), so that a cluster that was deleted loses its
// pause entry. Call it before the enforcer is used.
func (e *Enforcer) UseClusters(all func() (clusters []discovery.TenantCluster, ok bool)) {
	e.opts.AllClusters = all
}

// What became of a billing webhook: the result label of
// vbilling_webhooks_total.
const (
	// WebhookApplied: applied to the billing hold of its tenant(s) under
	// the hold rules (it raised the hold, cleared it, or changed nothing
	// because the hold is more severe).
	WebhookApplied = "applied"
	// WebhookRefused: a payment that lifted nothing, because a billing
	// platform still shows something owed. Answered 200.
	WebhookRefused = "refused"
	// WebhookUnsettled: a Metronome payment that found a threshold
	// notification still in alarm within an hour of the payment. Answered
	// with an error, so Metronome delivers it again.
	WebhookUnsettled = "unsettled"
	// WebhookDuplicate: the event was applied before.
	WebhookDuplicate = "duplicate"
	// WebhookIgnored: no billing signal in the event type, or the event is
	// older than the newest one applied to the hold.
	WebhookIgnored = "ignored"
	// WebhookUnknownCustomer: the customer is not a tenant of vBilling's.
	WebhookUnknownCustomer = "unknown_customer"
	// WebhookInvalid: the signature did not verify, or the body cannot be
	// read.
	WebhookInvalid = "invalid"
	// WebhookError: it could not be applied (a billing platform that cannot
	// be read, a state that cannot be stored, a cluster that cannot be
	// reached). Answered with an error, so the platform delivers it again.
	WebhookError = "error"
)

var webhookResults = []string{WebhookApplied, WebhookRefused, WebhookUnsettled, WebhookDuplicate, WebhookIgnored,
	WebhookUnknownCustomer, WebhookInvalid, WebhookError}

const webhooksMetric, webhooksHelp = "vbilling_webhooks_total", "Billing webhooks received, by billing platform and by what became of them."

// malformed marks a webhook whose body cannot be read.
type malformed struct{ err error }

func (m malformed) Error() string { return "decode: " + m.err.Error() }
func (m malformed) Unwrap() error { return m.err }

// WebhookResult names what became of a webhook, from what its handler
// returned.
func WebhookResult(sig Signal, err error) string {
	var bad malformed
	switch {
	case err == nil && sig.refused:
		return WebhookRefused
	case err == nil:
		return WebhookApplied
	case errors.Is(err, ErrNotSettled):
		return WebhookUnsettled
	case errors.Is(err, ErrDuplicate):
		return WebhookDuplicate
	case errors.Is(err, ErrIgnored):
		return WebhookIgnored
	case errors.Is(err, ErrUnknownCustomer):
		return WebhookUnknownCustomer
	case errors.Is(err, ErrSignature), errors.As(err, &bad):
		return WebhookInvalid
	}
	return WebhookError
}

// countWebhook counts one webhook of a billing platform.
func (e *Enforcer) countWebhook(source string, sig Signal, err error) {
	e.opts.Telemetry.Add(webhooksMetric, webhooksHelp, map[string]string{"source": source, "result": WebhookResult(sig, err)}, 1)
}

// stateMetric is the gauge of every tenant's billing state.
const stateMetric, stateHelp = "vbilling_tenant_billing_state", "1 for the tenant's current billing state."

var allStates = []State{Active, Warning, Delinquent, Suspended, Paused}

// exportState sets the tenant's series: 1 for its state, 0 for the others.
func (e *Enforcer) exportState(tenant string, state State) {
	state = orActive(state)
	e.gmu.Lock()
	defer e.gmu.Unlock()
	if cur, ok := e.exported[tenant]; ok && cur == state {
		return
	}
	e.exported[tenant] = state
	for _, s := range allStates {
		v := 0.0
		if s == state {
			v = 1
		}
		e.opts.Telemetry.Set(stateMetric, stateHelp, map[string]string{"tenant": tenant, "state": string(s)}, v)
	}
}

// syncStateMetric keeps the gauge in step with the tenants vBilling knows:
// every tenant with a hold, and every tenant discovery knows (active when
// it has no hold). A tenant that was offboarded and has no hold loses its
// series. Without a list of tenants (UseTenants), every tenant with a
// recorded state keeps one.
func (e *Enforcer) syncStateMetric() {
	want := map[string]State{}
	for _, ts := range e.States() {
		if len(ts.Holds) > 0 || e.opts.Tenants == nil {
			want[ts.Tenant] = orActive(ts.State)
		}
	}
	if e.opts.Tenants != nil {
		for _, tenant := range e.opts.Tenants() {
			if _, ok := want[tenant]; !ok && tenant != "" {
				want[tenant] = e.StateOf(tenant).State
			}
		}
	}
	e.gmu.Lock()
	var gone []string
	for tenant := range e.exported {
		if _, ok := want[tenant]; !ok {
			gone = append(gone, tenant)
		}
	}
	for _, tenant := range gone {
		delete(e.exported, tenant)
		for _, s := range allStates {
			e.opts.Telemetry.Delete(stateMetric, map[string]string{"tenant": tenant, "state": string(s)})
		}
	}
	e.gmu.Unlock()
	for tenant, state := range want {
		e.exportState(tenant, state)
	}
}

// knownTenant reports whether discovery knows a tenant, or nothing says
// which tenants it knows.
func (e *Enforcer) knownTenant(tenant string) bool {
	if e.opts.Tenants == nil {
		return true
	}
	for _, t := range e.opts.Tenants() {
		if t == tenant {
			return true
		}
	}
	return false
}

// unlockNote is a payment that did not lift its tenant's billing hold.
type unlockNote struct {
	At        time.Time `json:"at"` // when vBilling received it
	Source    string    `json:"source"`
	EventType string    `json:"event_type"`
	Result    string    `json:"result"` // WebhookRefused or WebhookUnsettled
	Why       string    `json:"why"`
}

const (
	unlockPrefix = "unlock/"
	unlockKind   = "billing-unlock"
	accessKind   = "enforcement-access"
	// pauseKind is the kind of the source pause/<cluster>: a paused cluster
	// whose stop could not be verified.
	pauseKind = "pause"
)

// pauseHint says what an unverified pause means and where the reasons are
// explained.
const pauseHint = "its control plane is stopped, but vBilling could not confirm that nothing of the cluster runs: its own nodes, and whatever is left on them, keep billing. " +
	"See When a pause cannot be verified in docs/private-nodes.html"

// reportPauses reports every paused cluster whose stop vBilling could not
// verify as the source pause/<cluster>, failing with the reason: its tenant
// is paused, yet something of the cluster may still run and its capacity
// keeps billing, which an operator otherwise sees only in GET
// /api/v1/billing-states. A pause that was verified since, or lifted, or
// whose cluster is gone, is no longer reported.
func (e *Enforcer) reportPauses() {
	if e.opts.Health == nil {
		return
	}
	now := map[string]string{}
	for _, st := range e.Pauses() {
		if st.Phase == "paused" && !st.Verified && st.Unverified != "" {
			now[st.Cluster] = st.Unverified
		}
	}
	e.omu.Lock()
	var cleared []string
	for id := range e.unverified {
		if _, still := now[id]; !still {
			cleared = append(cleared, id)
			delete(e.unverified, id)
		}
	}
	ids := make([]string, 0, len(now))
	for id := range now {
		e.unverified[id] = true
		ids = append(ids, id)
	}
	e.omu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		e.opts.Health.Fail(health.Name(pauseKind, id), pauseKind, id, fmt.Errorf("paused, but the stop is not verified: %s", now[id]), pauseHint)
	}
	sort.Strings(cleared)
	for _, id := range cleared {
		e.opts.Health.Remove(health.Name(pauseKind, id))
	}
}

// loadUnlocks reads the payments that had not lifted a hold when vBilling
// last stopped: they stay visible across a restart.
func (e *Enforcer) loadUnlocks() {
	for tenant, raw := range e.opts.State.WithPrefix(unlockPrefix) {
		var n unlockNote
		if json.Unmarshal([]byte(raw), &n) == nil {
			e.unlocks[tenant] = n
		}
	}
}

// noteUnlock records a payment that did not lift a tenant's billing hold,
// and reports it as the source billing-unlock/<tenant> until the hold is
// lifted.
func (e *Enforcer) noteUnlock(tenant string, sig *Signal, result, why string) {
	n := unlockNote{At: sig.At, Source: sig.Source, EventType: sig.Type, Result: result, Why: why}
	e.omu.Lock()
	e.unlocks[tenant] = n
	e.omu.Unlock()
	if b, err := json.Marshal(n); err == nil {
		if err := e.opts.State.Set(unlockPrefix+tenant, string(b)); err != nil {
			log.Printf("[enforcement] tenant %s: store the payment that did not lift its hold: %v", tenant, err)
		}
	}
	e.reportUnlock(tenant, n)
}

func (e *Enforcer) reportUnlock(tenant string, n unlockNote) {
	if e.opts.Health == nil {
		return
	}
	hold := e.StateOf(tenant).Holds[HoldBilling]
	msg := fmt.Sprintf("a payment (%s %s, received %s) did not lift the billing hold (%s since %s): %s",
		n.Source, n.EventType, n.At.UTC().Format(time.RFC3339), orActive(hold.State), hold.Since.UTC().Format(time.RFC3339), n.Why)
	hint := "the hold lifts with the next payment once the billing platforms show nothing owed"
	if n.Result == WebhookUnsettled {
		hint = "the billing platform delivers the payment again, and the hold lifts once it shows nothing owed"
	}
	hint += fmt.Sprintf(`; to lift it now: PUT /api/v1/billing-states/%s {"state":"active","hold":"billing"}`, tenant)
	e.opts.Health.Fail(health.Name(unlockKind, tenant), unlockKind, tenant, errors.New(msg), hint)
}

// clearUnlock forgets the payment that did not lift a tenant's hold: the
// hold is lifted.
func (e *Enforcer) clearUnlock(tenant string) {
	e.omu.Lock()
	_, noted := e.unlocks[tenant]
	delete(e.unlocks, tenant)
	e.omu.Unlock()
	if !noted {
		return
	}
	if err := e.opts.State.Delete(unlockPrefix + tenant); err != nil {
		log.Printf("[enforcement] tenant %s: forget the payment that did not lift its hold: %v", tenant, err)
	}
	if e.opts.Health != nil {
		e.opts.Health.OK(health.Name(unlockKind, tenant), unlockKind, tenant, 0)
	}
}

// reportNotes reports again what is still wrong, so that a source that
// stays a problem for days is not forgotten (sources nothing reports for
// six hours are dropped), and clears what no longer holds.
func (e *Enforcer) reportNotes() {
	e.omu.Lock()
	unlocks := make(map[string]unlockNote, len(e.unlocks))
	for t, n := range e.unlocks {
		unlocks[t] = n
	}
	refused := make(map[string]string, len(e.refused))
	for id, msg := range e.refused {
		refused[id] = msg
	}
	e.omu.Unlock()
	tenants := make([]string, 0, len(unlocks))
	for t := range unlocks {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, t := range tenants {
		if h, held := e.StateOf(t).Holds[HoldBilling]; !held || orActive(h.State) == Active {
			e.clearUnlock(t)
			continue
		}
		e.reportUnlock(t, unlocks[t])
	}
	if e.opts.Health == nil {
		return
	}
	ids := make([]string, 0, len(refused))
	for id := range refused {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e.opts.Health.Fail(health.Name(accessKind, id), accessKind, id, errors.New(refused[id]), accessHint)
	}
	e.reportPauses()
}

// accessHint names the rule that grants what vBilling needs inside a
// cluster to suspend and pause it.
const accessHint = "inside the cluster, grant the account vBilling connects with (the ClusterRole vbilling-enforcer, bound to vbilling-reader): " +
	"create on validatingadmissionpolicies and validatingadmissionpolicybindings; get, update and delete on those named " + PolicyName + " and " + PausedPolicyName +
	"; get and update on nodes; delete on pods. See Enforcement in docs/private-nodes.html"

// refusedAccess reports whether a cluster's API refused vBilling itself:
// its credentials are not accepted (Unauthorized) or lack a permission
// (Forbidden). Trying again changes nothing until someone grants the
// access.
func refusedAccess(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)
}

// noteAccess records what a cluster's API answered to enforcement inside
// it. A refusal is logged once and reported as the source
// enforcement-access/<cluster> until a later pass gets through.
func (e *Enforcer) noteAccess(cl discovery.TenantCluster, err error) {
	id := cl.ExternalID()
	e.omu.Lock()
	prev, noted := e.refused[id]
	switch {
	case refusedAccess(err):
		e.refused[id] = err.Error()
	case err == nil:
		delete(e.refused, id)
	}
	e.omu.Unlock()
	switch {
	case refusedAccess(err):
		if !noted || prev != err.Error() {
			log.Printf("[enforcement] %s: the cluster's API refuses vBilling (%v): %s", id, err, accessHint)
		}
		if e.opts.Health != nil {
			e.opts.Health.Fail(health.Name(accessKind, id), accessKind, id, err, accessHint)
		}
	case err == nil && noted:
		if e.opts.Health != nil {
			e.opts.Health.OK(health.Name(accessKind, id), accessKind, id, 1)
		}
	}
}

// forgetObserved drops what is kept, to report it, about a cluster that
// was deleted: its API's refusal is no longer reported.
func (e *Enforcer) forgetObserved(id string) {
	e.omu.Lock()
	defer e.omu.Unlock()
	delete(e.refused, id)
	delete(e.nudgeWarned, id)
	if e.unverified[id] {
		delete(e.unverified, id)
		if e.opts.Health != nil {
			e.opts.Health.Remove(health.Name(pauseKind, id))
		}
	}
	for key := range e.installed {
		if strings.HasPrefix(key, id+"\x00") {
			delete(e.installed, key)
		}
	}
}

// Repairs of the admission policies vBilling keeps inside a cluster: the
// kind label of vbilling_enforcement_repairs_total.
const (
	RepairPolicyDeleted  = "policy_deleted"
	RepairPolicyChanged  = "policy_changed"
	RepairBindingDeleted = "binding_deleted"
	RepairBindingChanged = "binding_changed"
)

const repairsMetric, repairsHelp = "vbilling_enforcement_repairs_total", "Admission policies and bindings inside clusters that vBilling found deleted or weakened, and restored."

// repaired counts and logs a policy or binding vBilling put back: someone
// inside the cluster (its tenant's admins are cluster admins there) deleted
// or changed it while the tenant is suspended or paused.
func (e *Enforcer) repaired(cl discovery.TenantCluster, tenant, kind, what string) {
	e.opts.Telemetry.Add(repairsMetric, repairsHelp, map[string]string{"kind": kind}, 1)
	log.Printf("[enforcement] %s: tenant %s: %s inside the cluster: vBilling restored it. New pods could start while it was gone or changed", cl.ExternalID(), tenant, what)
}

// installedKey names one object vBilling installed in one cluster.
func installedKey(id, name, object string) string { return id + "\x00" + name + "\x00" + object }

func (e *Enforcer) wasInstalled(id, name, object string) bool {
	e.omu.Lock()
	defer e.omu.Unlock()
	return e.installed[installedKey(id, name, object)]
}

func (e *Enforcer) setInstalled(id, name string, installed bool) {
	e.omu.Lock()
	defer e.omu.Unlock()
	for _, object := range []string{"policy", "binding"} {
		if installed {
			e.installed[installedKey(id, name, object)] = true
		} else {
			delete(e.installed, installedKey(id, name, object))
		}
	}
}

// policyWeakened says how an admission policy found in a cluster no longer
// denies what vBilling's does ("" when it still does). Only what decides
// which pods are denied is compared: the message, which changes with the
// tenant's hold, and the API server's defaults never count.
func policyWeakened(want, p admissionv1.ValidatingAdmissionPolicySpec) string {
	same := len(p.Validations) == len(want.Validations)
	for i := 0; same && i < len(want.Validations); i++ {
		same = p.Validations[i].Expression == want.Validations[i].Expression
	}
	var wantSel *metav1.LabelSelector
	covered := true
	if want.MatchConstraints != nil {
		wantSel = want.MatchConstraints.NamespaceSelector
		for _, r := range want.MatchConstraints.ResourceRules {
			for _, op := range r.Operations {
				covered = covered && p.MatchConstraints != nil && coversPods(p.MatchConstraints.ResourceRules, op)
			}
		}
	}
	switch mc := p.MatchConstraints; {
	case p.FailurePolicy != nil && *p.FailurePolicy != admissionv1.Fail:
		return "its failure policy is " + string(*p.FailurePolicy)
	case p.ParamKind != nil:
		return "it takes parameters"
	case len(p.MatchConditions) > 0:
		return "it has match conditions"
	case mc == nil || !covered || !sameSelector(mc.NamespaceSelector, wantSel) || !emptySelector(mc.ObjectSelector) || len(mc.ExcludeResourceRules) > 0:
		return "it no longer matches the pods it must"
	case !same:
		return "its validation is not vBilling's"
	}
	return ""
}

// bindingWeakened says how a binding found in a cluster no longer makes its
// policy deny ("" when it still does).
func bindingWeakened(name string, b admissionv1.ValidatingAdmissionPolicyBindingSpec) string {
	switch {
	case b.PolicyName != name || b.ParamRef != nil:
		return "it names another policy or parameters"
	case !hasAction(b.ValidationActions, admissionv1.Deny):
		return "it does not deny"
	}
	if r := b.MatchResources; r != nil && (!emptySelector(r.NamespaceSelector) || !emptySelector(r.ObjectSelector) || len(r.ResourceRules) > 0 || len(r.ExcludeResourceRules) > 0) {
		return "it is narrowed to some pods"
	}
	return ""
}

func hasAction(actions []admissionv1.ValidationAction, a admissionv1.ValidationAction) bool {
	for _, v := range actions {
		if v == a {
			return true
		}
	}
	return false
}

// sameSelector reports whether two label selectors select the same
// objects, an unset one selecting everything.
func sameSelector(a, b *metav1.LabelSelector) bool {
	if emptySelector(a) || emptySelector(b) {
		return emptySelector(a) && emptySelector(b)
	}
	as, aerr := metav1.LabelSelectorAsSelector(a)
	bs, berr := metav1.LabelSelectorAsSelector(b)
	return aerr == nil && berr == nil && as.String() == bs.String()
}

// trimSettle shortens an ErrNotSettled error to what is still owed.
func trimSettle(err error) string {
	return strings.TrimPrefix(err.Error(), ErrNotSettled.Error()+": ")
}
