package metronome

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Price coverage after the live validation of v0.3.2: the check reads the
// rate card of the contract that rates a tenant's usage (in both Metronome
// modes), a tenant without customer or contract is unpriced, not unknown,
// and the log names what Metronome holds.

// logBuffer collects the process log.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// lines returns the log lines that contain every given string.
func (l *logBuffer) lines(containing ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.b.String(), "\n") {
		ok := line != ""
		for _, c := range containing {
			ok = ok && strings.Contains(line, c)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

const (
	h100 = "NVIDIA-H100-80GB-HBM3"
	b200 = "NVIDIA-B200"
)

// provisioned is a vBilling that creates its own objects in the fake: its
// billable metrics exist, with a GPU product priced per SKU, on a rate card
// with the alias payg-usd that prices the H100 and a second rate card,
// without alias, that prices the B200 only.
type provisioned struct {
	f        *metronometest.Server
	a        *Adapter
	old, new string // rate card IDs
}

func newProvisioned(t *testing.T, f *metronometest.Server, mutate ...func(*config.Config)) *provisioned {
	t.Helper()
	a := newReadOnly(t, f, append([]func(*config.Config){func(c *config.Config) {
		c.MetronomeProvision, c.MetronomeRateCard = true, "payg-usd"
	}}, mutate...)...)
	if err := a.Bootstrap(context.Background(), usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	bm, ok := a.state.Get("bm/" + usage.MetricGPUHours)
	if !ok {
		t.Fatal("no GPU billable metric after the bootstrap")
	}
	prod := f.AddProduct("GPU Hours", bm, "sku")
	p := &provisioned{f: f, a: a, old: f.AddRateCard("PAYG 2026", "payg-usd"), new: f.AddRateCard("PAYG 2027")}
	f.AddRate(metronometest.Rate{RateCardID: p.old, ProductID: prod, Price: 190, PricingGroupValues: map[string]string{"sku": h100}})
	f.AddRate(metronometest.Rate{RateCardID: p.new, ProductID: prod, Price: 240, PricingGroupValues: map[string]string{"sku": b200}})
	return p
}

// later moves the adapter's clock past the time after which a tenant's
// contracts are read again.
func (p *provisioned) later(n int) {
	at := now.Add(time.Duration(n) * (planRefresh + time.Minute))
	p.a.now = func() time.Time { return at }
}

func seriesOf(a *Adapter, t usage.Tenant, sku string) destinations.PriceSeries {
	ev := usage.Event{Tenant: t.ID, Metric: usage.MetricGPUHours, SKU: sku, Region: "ap-southeast-2",
		Dimensions: map[string]string{usage.DimCapacityType: usage.CapacityOnDemand}}
	return destinations.SeriesOf(a, a.PricePlan(t), &ev)
}

// Live finding (CODE-001): with METRONOME_PROVISION a rate card alias was
// resolved again by every price check, while a tenant's contract keeps the
// rate card it was created on. After the alias moved to an empty rate card,
// every series read unpriced and 20 events were held, although the tenant's
// contract was still on the old, fully priced rate card; a SKU priced on the
// new rate card only read priced and was rated at 0. The check reads the
// contract's rate card now, and says that the alias moved.
func TestProvisionModeChecksTheRateCardOfTheTenantsContract(t *testing.T) {
	logs := captureLog(t)
	f := newFake(t)
	p := newProvisioned(t, f)
	a, ctx := p.a, context.Background()
	acme := usage.Tenant{ID: "tenant-acme"}

	// Before vBilling created the tenant's contract, its usage is checked
	// against the rate card the alias points to: the contract will be there.
	got := coverageOf(t, a, seriesOf(a, acme, h100), seriesOf(a, acme, b200))
	if got[0].Status != destinations.PriceCovered || got[1].Status != destinations.PriceMissing {
		t.Fatalf("before the contract exists: H100 %+v, B200 %+v", got[0], got[1])
	}
	if plan := a.PricePlan(acme); plan != p.old {
		t.Fatalf("plan %q, want the rate card the alias points to (%s)", plan, p.old)
	}
	if err := a.EnsureTenant(ctx, acme); err != nil {
		t.Fatal(err)
	}
	cus, _ := a.state.Get("cus/tenant-acme")
	contracts := f.Contracts(cus)
	if len(contracts) != 1 || contracts[0].RateCardID != p.old {
		t.Fatalf("contracts %+v, want one on the rate card the alias pointed to", contracts)
	}
	contract := contracts[0].ID
	got = coverageOf(t, a, seriesOf(a, acme, h100))
	if got[0].Status != destinations.PriceCovered || got[0].Note != "" || a.PricePlan(acme) != p.old {
		t.Fatalf("with its contract: %+v, plan %q", got[0], a.PricePlan(acme))
	}
	if l := logs.lines("tenant tenant-acme: customer "+cus, "usage rated on contract "+contract+` on rate card "payg-usd" (`+p.old+")"); len(l) != 1 {
		t.Fatalf("the log names the customer, the contract and the rate card ID once: %v", logs.lines("tenant tenant-acme:"))
	}

	// Finance points the alias at next year's prices. The contract stays.
	f.MoveRateCardAlias("payg-usd", p.new)
	for i := 1; i <= 2; i++ {
		p.later(i)
		got = coverageOf(t, a, seriesOf(a, acme, h100), seriesOf(a, acme, b200))
	}
	if c := got[0]; c.Status != destinations.PriceCovered || c.Series.Plan != p.old || !strings.Contains(c.Detail, "flat 190") {
		t.Fatalf("H100 is rated on the contract's rate card, where it has a rate: %+v", c)
	}
	if c := got[1]; c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, "sku="+b200) {
		t.Fatalf("B200 has a rate only on the rate card the alias points to now, not on the contract's: %+v", c)
	}
	for _, c := range got {
		for _, want := range []string{`rate card setting "payg-usd" points to rate card "payg-usd" (` + p.new + ") now",
			"contract " + contract + " of tenant tenant-acme is on rate card", "(" + p.old + ")",
			"only when the tenant's rate card setting changes", "00:00 UTC"} {
			if !strings.Contains(c.Note, want) {
				t.Errorf("note of %s lacks %q: %q", c.Series.SKU, want, c.Note)
			}
		}
	}
	if l := logs.lines("WARNING: tenant tenant-acme:", `rate card setting "payg-usd" points to`, p.new, p.old, contract); len(l) != 1 {
		t.Fatalf("the move is logged once, with both rate cards and the contract: %v", logs.lines("WARNING"))
	}

	// A tenant that is new now gets its contract where the alias points now.
	fresh := usage.Tenant{ID: "tenant-fresh"}
	got = coverageOf(t, a, seriesOf(a, fresh, h100), seriesOf(a, fresh, b200))
	if got[0].Status != destinations.PriceMissing || got[1].Status != destinations.PriceCovered || got[0].Note != "" {
		t.Fatalf("a new tenant is checked against the alias's rate card: H100 %+v, B200 %+v", got[0], got[1])
	}
	if plan := a.PricePlan(fresh); plan != p.new {
		t.Fatalf("plan of the new tenant %q, want %s", plan, p.new)
	}
	if plan := a.PricePlan(usage.Tenant{}); plan != p.new {
		t.Fatalf("billable metrics without usage are checked under %q, want the rate card new tenants get (%s)", plan, p.new)
	}

	// The alias is moved back: said once, and the note goes.
	f.MoveRateCardAlias("payg-usd", p.old)
	for i := 3; i <= 4; i++ {
		p.later(i)
		got = coverageOf(t, a, seriesOf(a, acme, h100))
	}
	if got[0].Status != destinations.PriceCovered || got[0].Note != "" {
		t.Fatalf("after the alias moved back: %+v", got[0])
	}
	if l := logs.lines("tenant tenant-acme:", `"payg-usd" points to the rate card of its contract again`); len(l) != 1 {
		t.Fatalf("log after the alias moved back: %v", logs.lines("tenant tenant-acme:"))
	}
	if w := f.Writes(); strings.Count(strings.Join(w, ","), "/v1/contracts/") != 1 {
		t.Fatalf("the price check never creates or ends a contract: %v", w)
	}
}

// The same finding, as vBilling runs: with PRICE_CHECK=hold, usage of a
// tenant whose contract is on the priced rate card keeps shipping after the
// alias moved to an empty one (v0.3.2 held it). A tenant that is new after
// the move gets its contract on the empty rate card, so its usage is held.
func TestMovingARateCardAliasHoldsNoUsageTheContractStillRates(t *testing.T) {
	f := newRigFake(t)
	priced, empty := f.AddRateCard("PAYG 2026", "payg-usd"), f.AddRateCard("PAYG 2027")
	r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL, provision: true, rateCard: "payg-usd", mode: pricing.ModeHold})
	waitFor(t, "the billable metrics created", func() bool { st := r.disp.Statuses(); return len(st) == 1 && st[0].Ready })
	bm, _ := r.a.state.Get("bm/" + usage.MetricGPUHours)
	prod := f.AddProduct("GPU Hours", bm, "sku")
	f.AddRate(metronometest.Rate{RateCardID: priced, ProductID: prod, Price: 190, PricingGroupValues: map[string]string{"sku": h100}})
	r.check()

	at := rigNow()
	before := gpu("tenant-acme", "c1", at.Add(-5*time.Minute), 1)
	r.commit(t, before)
	waitFor(t, "the tenant's first usage delivered", func() bool { return r.delivered() && sentIDs(f)[before.ID] && r.held.Count(rigDest) == 0 })
	cus, _ := r.a.state.Get("cus/tenant-acme")
	if c := f.Contracts(cus); len(c) != 1 || c[0].RateCardID != priced {
		t.Fatalf("contracts %+v, want one on the rate card the alias pointed to", c)
	}

	f.MoveRateCardAlias("payg-usd", empty)
	r.check()
	after := gpu("tenant-acme", "c1", at.Add(-4*time.Minute), 1)
	r.commit(t, after)
	waitFor(t, "the usage after the alias moved delivered or held", func() bool { return r.delivered() })
	if !sentIDs(f)[after.ID] || r.held.Count(rigDest) != 0 {
		t.Fatalf("usage rated on the tenant's contract was held (%d) because the alias points to another rate card now", r.held.Count(rigDest))
	}
	r.check()
	rows := 0
	for _, d := range r.svc.Report().Destinations {
		for _, row := range d.Series {
			if row.Metric != usage.MetricGPUHours || row.NoUsageYet {
				continue
			}
			rows++
			if row.Plan != priced || row.Status != destinations.PriceCovered {
				t.Errorf("row %+v, want priced on the contract's rate card %s", row, priced)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("%d GPU rows with usage, want the tenant's one", rows)
	}
	// (The row's note about the alias appears when the tenant's contract is
	// read again, at most 5 minutes later:
	// TestProvisionModeChecksTheRateCardOfTheTenantsContract.)

	late := gpu("tenant-late", "c1", at.Add(-3*time.Minute), 1)
	r.commit(t, late)
	waitFor(t, "the new tenant's usage held", func() bool { return r.delivered() && r.held.Count(rigDest) == 1 })
	if sentIDs(f)[late.ID] {
		t.Fatal("usage of a tenant whose contract is on the empty rate card was delivered")
	}
}

// The tenant moves to the rate card its setting points to only under the
// adapter's contract rules: when the setting changes, vBilling starts a new
// contract at UTC midnight and ends the old one there. The check follows.
func TestProvisionModeFollowsTheTenantToItsNewContract(t *testing.T) {
	f := newFake(t)
	p := newProvisioned(t, f)
	a, ctx := p.a, context.Background()
	acme := usage.Tenant{ID: "tenant-acme"}
	if err := a.EnsureTenant(ctx, acme); err != nil {
		t.Fatal(err)
	}
	coverageOf(t, a, seriesOf(a, acme, h100))
	f.MoveRateCardAlias("payg-usd", p.new)

	// The operator sets the tenant's plan to the new rate card's ID.
	p.a.now = func() time.Time { return now.AddDate(0, 0, 1) }
	f.SetClock(p.a.now)
	acme.Plan = p.new
	if plan := a.PricePlan(acme); plan != aliasPlan+"tenant-acme" {
		t.Fatalf("plan %q right after the setting changed: the next check must read the tenant again", plan)
	}
	// Before EnsureTenant: the contract will be on the new rate card.
	got := coverageOf(t, a, seriesOf(a, acme, b200))
	if got[0].Status != destinations.PriceCovered {
		t.Fatalf("B200 before the new contract exists: %+v", got[0])
	}
	if err := a.EnsureTenant(ctx, acme); err != nil {
		t.Fatal(err)
	}
	got = coverageOf(t, a, seriesOf(a, acme, b200), seriesOf(a, acme, h100))
	if got[0].Status != destinations.PriceCovered || got[1].Status != destinations.PriceMissing || got[0].Note != "" || a.PricePlan(acme) != p.new {
		t.Fatalf("on the new contract: B200 %+v, H100 %+v, plan %q", got[0], got[1], a.PricePlan(acme))
	}
	cus, _ := a.state.Get("cus/tenant-acme")
	open := 0
	for _, c := range f.Contracts(cus) {
		if !c.Archived && c.EndingBefore.IsZero() {
			open++
			if c.RateCardID != p.new || !c.StartingAt.Equal(now.AddDate(0, 0, 1).Truncate(24*time.Hour)) {
				t.Errorf("open contract %+v, want one on the new rate card from UTC midnight", c)
			}
		}
	}
	if open != 1 {
		t.Fatalf("%d open contracts: %+v", open, f.Contracts(cus))
	}
}

// With METRONOME_PROVISION and no rate card, vBilling creates customers and
// no contracts: nothing rates the usage until someone adds a contract. That
// was "unknown" (delivered in hold mode); it is unpriced, and the check
// reads the contract the operator adds.
func TestProvisionModeWithoutARateCardReadsTheContractsTheOperatorAdds(t *testing.T) {
	f := newFake(t)
	p := newProvisioned(t, f, func(c *config.Config) { c.MetronomeRateCard = "" })
	a, ctx := p.a, context.Background()
	acme := usage.Tenant{ID: "tenant-acme"}
	got := coverageOf(t, a, seriesOf(a, acme, h100))
	if c := got[0]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "no rate card is configured") || !strings.Contains(c.Hint, "METRONOME_RATE_CARD") {
		t.Fatalf("before the customer exists: %+v", c)
	}
	if err := a.EnsureTenant(ctx, acme); err != nil {
		t.Fatal(err)
	}
	cus, _ := a.state.Get("cus/tenant-acme")
	got = coverageOf(t, a, seriesOf(a, acme, h100))
	if c := got[0]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "customer "+cus) || !strings.Contains(c.Detail, "no active contract") {
		t.Fatalf("a customer without contract: %+v", c)
	}
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: p.old, StartingAt: now.AddDate(0, 0, -1)})
	got = coverageOf(t, a, seriesOf(a, acme, h100))
	if c := got[0]; c.Status != destinations.PriceCovered || a.PricePlan(acme) != p.old {
		t.Fatalf("with the operator's contract: %+v, plan %q", c, a.PricePlan(acme))
	}
	if plan := a.PricePlan(usage.Tenant{}); plan != "" {
		t.Fatalf("plan of no tenant %q: there is no default rate card", plan)
	}
}

// A tenant whose customer or contract vBilling could not create for good is
// delivered without anything to rate it (the dispatcher logs and goes on).
// The price check says so: unpriced, with the error, until it works.
func TestProvisionModeReportsATenantWhoseContractCouldNotBeCreated(t *testing.T) {
	f := newFake(t)
	p := newProvisioned(t, f)
	a, ctx := p.a, context.Background()
	old, _ := f.CreateCustomer("Churned", "tenant-old")
	f.ArchiveCustomer(old)
	tenant := usage.Tenant{ID: "tenant-old"}
	if c := coverageOf(t, a, seriesOf(a, tenant, h100))[0]; c.Status != destinations.PriceCovered {
		t.Fatalf("before vBilling tried: %+v", c)
	}
	if err := a.EnsureTenant(ctx, tenant); err == nil || !destinations.IsPermanent(err) {
		t.Fatalf("want a permanent error for an alias an archived customer holds, got %v", err)
	}
	if plan := a.PricePlan(tenant); plan != aliasPlan+"tenant-old" {
		t.Fatalf("plan %q after the failure: the tenant's next usage must be checked again before it ships", plan)
	}
	c := coverageOf(t, a, seriesOf(a, tenant, h100))[0]
	if c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "could not create the customer or contract") || !strings.Contains(c.Detail, old) {
		t.Fatalf("after the permanent failure: %+v", c)
	}
	// The operator removes the alias from the archived customer.
	if err := f.SetIngestAliases(old); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if c := coverageOf(t, a, seriesOf(a, tenant, h100))[0]; c.Status != destinations.PriceCovered {
		t.Fatalf("after the customer and contract were created: %+v", c)
	}
}

// Live findings: the contract log lines named only the configured alias,
// never the rate card Metronome resolved it to, and said a contract "ended
// at" UTC midnight when it had been archived. They name the customer, the
// rate card ID Metronome holds and the alias, and say ended or archived.
func TestContractLogLinesSayWhatMetronomeHolds(t *testing.T) {
	logs := captureLog(t)
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg" })
	ctx := context.Background()
	ensure := func(ad *Adapter, tenant, plan string) {
		t.Helper()
		if err := ad.EnsureTenant(ctx, usage.Tenant{ID: tenant, Plan: plan}); err != nil {
			t.Fatal(err)
		}
	}
	one := func(what string, containing ...string) {
		t.Helper()
		if l := logs.lines(containing...); len(l) != 1 {
			t.Fatalf("%s: want one log line with %q, got %q (contract lines: %q)", what, containing, l, logs.lines("contract"))
		}
	}
	cus := uuidFor("acme")

	ensure(a, "acme", "")
	payg := uuidFor(f.contract(t, "payg", "2026-10-05"))
	one("a contract created on an alias", "contract "+payg+" for tenant acme (customer "+cus+") on rate card "+cardFor("payg")+` (alias "payg") from 2026-10-05`)

	// The tenant moves to another rate card the same day: the contract that
	// started today never rates anything, and is archived, not ended.
	ensure(a, "acme", "committed")
	committed := uuidFor(f.contract(t, "committed", "2026-10-05"))
	one("an archived contract", "contract "+payg+" of tenant acme (customer "+cus+", on rate card "+cardFor("payg")+") archived",
		"contract "+committed+" takes over from 2026-10-05T00:00:00Z")
	if l := logs.lines("contract "+payg+" of tenant", "ended at"); len(l) != 0 {
		t.Fatalf("an archived contract is logged as ended: %q", l)
	}

	// The next day it moves back: the contract that ran yesterday ends at
	// UTC midnight, where the new one starts.
	a.now = func() time.Time { return now.AddDate(0, 0, 1) }
	ensure(a, "acme", "")
	payg2 := uuidFor(f.contract(t, "payg", "2026-10-06"))
	one("an ended contract", "contract "+committed+" of tenant acme (customer "+cus+", on rate card "+cardFor("committed")+") ended at 2026-10-06T00:00:00Z, where contract "+payg2+" takes over")
	if l := logs.lines("contract "+committed+" of tenant", "archived"); len(l) != 0 {
		t.Fatalf("an ended contract is logged as archived: %q", l)
	}

	// A vBilling that lost its state keeps the open contract, and says which.
	b := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg" })
	b.now = a.now
	ensure(b, "acme", "")
	one("a contract kept", "tenant acme (customer "+cus+") keeps contract "+payg2+" on rate card "+cardFor("payg")+` (alias "payg"), which started at 2026-10-06T00:00:00Z`)

	// A rate card configured by its ID needs no alias.
	rc := "8f0e3c1a-6d3b-4c8e-9a51-1f0d7a2b9c44"
	ensure(a, "beta", rc)
	one("a contract created on a rate card ID", "for tenant beta (customer "+uuidFor("beta")+") on rate card "+rc+" from 2026-10-06")
}

// The check reads every tenant's contracts. A customer Metronome does not
// know (a stale pin, a customer deleted there) is that tenant's gap, reported
// on its series: it never fails the check for the other tenants.
func TestACustomerMetronomeDoesNotKnowIsThatTenantsGapOnly(t *testing.T) {
	f := newFake(t)
	p := newProvisioned(t, f)
	a, ctx := p.a, context.Background()
	const gone = "0b0b0b0b-0000-4000-8000-00000000dead"
	// Metronome answers 404 for the contracts of a customer it does not have.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v2/contracts/list" && bytes.Contains(body, []byte(gone)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"customer not found"}`)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, f.URL+r.URL.RequestURI(), bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(front.Close)
	a.c = NewClient(front.URL, metronometest.Token)

	acme := usage.Tenant{ID: "tenant-acme"}
	if err := a.EnsureTenant(ctx, acme); err != nil {
		t.Fatal(err)
	}
	// vBilling was pinned to a customer for this tenant and created its
	// contract; the customer was deleted in Metronome since.
	lost := usage.Tenant{ID: "tenant-lost", ProviderIDs: map[string]string{"metronome": gone}}
	if err := a.state.Set("contract-current/tenant-lost", "payg-usd"); err != nil {
		t.Fatal(err)
	}
	got := coverageOf(t, a, seriesOf(a, acme, h100), seriesOf(a, lost, h100))
	if got[0].Status != destinations.PriceCovered {
		t.Fatalf("the other tenant is still checked: %+v", got[0])
	}
	if c := got[1]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "Metronome has no customer "+gone) || !strings.Contains(c.Hint, "metronome-customer-id") {
		t.Fatalf("the tenant whose customer is gone: %+v", c)
	}
}

// catalog is Finance's side of a Metronome account vBilling only reads: the
// GPU billable metric, a product priced per SKU, and a rate card with a rate
// for the H100.
func catalog(t *testing.T, f *metronometest.Server) (card string) {
	t.Helper()
	bm := financeMetric(t, f, usage.MetricGPUHours, "GPU Hours")
	prod := f.AddProduct("GPU Hours", bm, "sku")
	card = f.AddRateCard("List")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: prod, Price: 300, PricingGroupValues: map[string]string{"sku": h100}})
	return card
}

// Live finding: with PRICE_CHECK=hold a tenant whose customer or contract
// the operator's portal forgot was reported unknown and delivered unrated.
// Nothing rates that usage, so it is held, and released in ledger order,
// before newer usage, once the customer and a contract exist.
func TestHoldModeHoldsUsageUntilTheTenantHasACustomerAndAContract(t *testing.T) {
	f := newRigFake(t)
	card := catalog(t, f)
	r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL, mode: pricing.ModeHold})
	at := rigNow()
	first := gpu("tenant-newbie", "c1", at.Add(-5*time.Minute), 1)
	r.commit(t, first)
	waitFor(t, "the usage of a tenant without customer held", func() bool { return r.delivered() && r.held.Count(rigDest) == 1 })
	if len(f.Events()) != 0 {
		t.Fatal("usage nothing rates reached Metronome although PRICE_CHECK=hold")
	}
	row := func() pricing.Row {
		t.Helper()
		var rows []pricing.Row
		for _, d := range r.svc.Report().Destinations {
			for _, x := range d.Series {
				if x.Metric == usage.MetricGPUHours && !x.NoUsageYet {
					rows = append(rows, x)
				}
			}
		}
		if len(rows) != 1 {
			t.Fatalf("GPU rows with usage: %+v", rows)
		}
		return rows[0]
	}
	if x := row(); x.Status != destinations.PriceMissing || x.Uncounted || x.HeldEvents != 1 || !strings.Contains(x.Detail, "no customer for this alias yet") ||
		!strings.Contains(x.Hint, `ingest alias "tenant-newbie"`) || x.Dims[usage.DimCapacityType] != usage.CapacityOnDemand {
		t.Fatalf("row of the held series: %+v", x)
	}
	r.check() // as the check a minute later does, while usage is held
	unpriced := false
	for _, src := range r.h.Problems() {
		unpriced = unpriced || (src.Kind == "pricing" && src.Status == health.Unpriced && strings.Contains(src.Name, "alias:tenant-newbie/"+usage.MetricGPUHours) &&
			strings.Contains(src.Message, "1 event(s) held"))
	}
	if !unpriced {
		t.Fatalf("no unpriced source for the tenant without customer: %+v", r.h.Problems())
	}

	// The portal creates the customer, without a contract yet: still unrated.
	cus, err := f.CreateCustomer("Newbie", "org_0b0b", "tenant-newbie")
	if err != nil {
		t.Fatal(err)
	}
	r.check()
	second := gpu("tenant-newbie", "c1", at.Add(-4*time.Minute), 1)
	r.commit(t, second)
	waitFor(t, "newer usage held behind it", func() bool { return r.delivered() && r.held.Count(rigDest) == 2 })
	if x := row(); x.Status != destinations.PriceMissing || !strings.Contains(x.Detail, "customer "+cus) || !strings.Contains(x.Detail, "no active contract") ||
		!strings.Contains(x.Hint, "add a contract for customer "+cus) {
		t.Fatalf("row with a customer and no contract: %+v", x)
	}
	if len(f.Events()) != 0 {
		t.Fatal("usage reached Metronome before a contract could rate it")
	}

	// The contract appears: what was held ships, in order, before newer usage.
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: at.AddDate(0, 0, -1)})
	r.check()
	third := gpu("tenant-newbie", "c1", at.Add(-3*time.Minute), 1)
	r.commit(t, third)
	waitFor(t, "the held usage released and the newer usage sent", func() bool { return r.delivered() && r.held.Count(rigDest) == 0 && len(f.Events()) == 3 })
	var order []string
	for _, e := range f.Events() {
		order = append(order, e.TransactionID)
		if e.Customer != cus {
			t.Errorf("event %s is not attributed to the tenant's customer", e.TransactionID)
		}
	}
	if order[0] != first.ID || order[1] != second.ID || order[2] != third.ID {
		t.Fatalf("sent %v, want the held events in ledger order, then the newer one", order)
	}
	if st := r.disp.Statuses()[0]; st.Released < 2 || st.ReleasedUnpriced != 0 || st.DeadLettered != 0 {
		t.Fatalf("status %+v: want the held events released with a price", st)
	}
	if got := r.counted(t, "tenant-newbie", usage.MetricGPUHours, at); got != 3 {
		t.Fatalf("Metronome counts %v GPU-hours for the customer, want all 3", got)
	}
}

// Usage held for want of a customer is released, unpriced, after the longest
// hold, like any held usage: before its invoice period can be final, with a
// failing source that says what to create. Younger usage stays held.
func TestUsageHeldForAMissingCustomerIsReleasedAfterTheLongestHold(t *testing.T) {
	f := newRigFake(t)
	catalog(t, f)
	r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL, mode: pricing.ModeHold, maxHold: time.Hour})
	at := rigNow()
	old, young := gpu("tenant-nobody", "c1", at.Add(-3*time.Hour), 1), gpu("tenant-nobody", "c1", at.Add(-10*time.Minute), 1)
	r.commit(t, old, young)
	waitFor(t, "the old usage released without a price", func() bool {
		return sentIDs(f)[old.ID] && r.held.Count(rigDest) == 1 && r.disp.Statuses()[0].ReleasedUnpriced == 1
	})
	if sentIDs(f)[young.ID] {
		t.Fatal("usage younger than the longest hold was released")
	}
	waitFor(t, "the failing source", func() bool {
		for _, src := range r.h.Problems() {
			if src.Kind == "pricing" && src.Status == health.Failing && strings.Contains(src.Message, "WITHOUT A PRICE") &&
				strings.Contains(src.Message, "no customer for this alias yet") && strings.Contains(src.Hint, `create a Metronome customer with the ingest alias "tenant-nobody"`) {
				return true
			}
		}
		return false
	})
}

// With PRICE_CHECK=warn the same tenant's usage is delivered, as Metronome
// keeps usage sent under an alias for the customer that gets it later, and
// counted as unpriced, so the shipped alert on vbilling_unpriced_series fires.
func TestWarnModeDeliversAndCountsUsageOfATenantWithoutCustomer(t *testing.T) {
	f := newFake(t)
	catalog(t, f)
	a := newReadOnly(t, f)
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	svc := pricing.New(sp, []destinations.Destination{a}, nil, nil, h, tel, pricing.Options{Mode: pricing.ModeWarn, DecideWait: 10 * time.Second})
	ev := gpu("tenant-newbie", "c1", now.Add(-time.Minute), 1)
	if dec := svc.Gate().Decide(context.Background(), rigDest, []usage.Event{ev}); dec[0].Hold {
		t.Fatalf("warn mode holds only usage no billable metric counts: %+v", dec[0])
	}
	rep := svc.Report()
	var row *pricing.Row
	for i, x := range rep.Destinations[0].Series {
		if x.Metric == usage.MetricGPUHours && !x.NoUsageYet {
			row = &rep.Destinations[0].Series[i]
		}
	}
	if row == nil || row.Status != destinations.PriceMissing || row.Uncounted || !strings.Contains(row.Hint, `ingest alias "tenant-newbie"`) {
		t.Fatalf("row of the tenant without customer: %+v", row)
	}
	if v := tel.Get("vbilling_unpriced_series", map[string]string{"destination": rigDest}); v < 1 || v != float64(rep.Unpriced) {
		t.Fatalf("vbilling_unpriced_series = %v, want the report's %d unpriced series, this one among them", v, rep.Unpriced)
	}
	source := false
	for _, src := range h.Problems() {
		source = source || (src.Status == health.Unpriced && strings.Contains(src.Name, "alias:tenant-newbie/"+usage.MetricGPUHours))
	}
	if !source {
		t.Fatalf("no unpriced source for the series: %+v", h.Problems())
	}
}

// Live finding: in the default mode no log line named the customer and the
// contract a price check found for a tenant, or recorded the change from no
// customer to no contract to a contract on a rate card. Each is logged once,
// with the IDs Metronome holds.
func TestDefaultModeLogsWhereATenantsUsageIsRated(t *testing.T) {
	logs := captureLog(t)
	f := newFake(t)
	card := catalog(t, f)
	a := newReadOnly(t, f)
	check := func() destinations.PriceCoverage {
		t.Helper()
		var c destinations.PriceCoverage
		for i := 0; i < 2; i++ { // a second check has nothing new to say
			c = coverageOf(t, a, seriesOf(a, usage.Tenant{ID: "tenant-newbie"}, h100))[0]
		}
		return c
	}
	all := func() []string { return logs.lines("[metronome] tenant tenant-newbie:") }

	check()
	if l := all(); len(l) != 1 || !strings.Contains(l[0], `no Metronome customer has the ingest alias "tenant-newbie", so nothing rates its usage`) {
		t.Fatalf("log without a customer: %v", l)
	}
	cus, _ := f.CreateCustomer("Newbie", "org_0b0b", "tenant-newbie")
	check()
	if l := all(); len(l) != 2 || !strings.Contains(l[1], "customer "+cus+" has no active contract with a rate card") || !strings.Contains(l[1], "(before: no Metronome customer") {
		t.Fatalf("log with a customer and no contract: %v", l)
	}
	contract := f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: now.Add(-time.Hour)})
	if c := check(); c.Status != destinations.PriceCovered {
		t.Fatalf("with a contract: %+v", c)
	}
	l := all()
	if len(l) != 3 || !strings.Contains(l[2], "customer "+cus+", usage rated on contract "+contract+` on rate card "List" (`+card+")") ||
		!strings.Contains(l[2], "(before: customer "+cus+" has no active contract") {
		t.Fatalf("log with a contract: %v", l)
	}
	// The contract ends: said once, and the usage is unpriced again.
	f.EndContract(contract, now.Add(-time.Minute))
	a.now = func() time.Time { return now.Add(planRefresh + time.Minute) }
	f.SetClock(a.now)
	check()
	if c := check(); c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "no active contract") {
		t.Fatalf("after the contract ended: %+v", c)
	}
	if l := all(); len(l) != 4 || !strings.Contains(l[3], "has no active contract with a rate card") || !strings.Contains(l[3], "(before: customer "+cus+", usage rated on contract "+contract) {
		t.Fatalf("log after the contract ended: %v", l)
	}
}
