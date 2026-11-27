package metronome

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// pricingKeys sets PRICING_KEYS for one test and clears it after.
func pricingKeys(t *testing.T, spec string) {
	t.Helper()
	keys, err := usage.ParsePricingKeys(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := usage.SetPricingKeys(keys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.SetPricingKeys(nil) })
}

func lookup(t *testing.T, code string) usage.MetricDef {
	t.Helper()
	def, ok := usage.Lookup(code)
	if !ok {
		t.Fatalf("no metric %s", code)
	}
	return def
}

// The billable metric of a metric with extra pricing keys groups on them and
// requires each, under a name that says so; the others keep their names
// and keys.
func TestBillableMetricGroupsOnThePricingKeys(t *testing.T) {
	pricingKeys(t, "vcluster_private_node_hours:zone|public_ip,vcluster_cpu_core_hours:sku")
	bm := BillableMetricFor(lookup(t, usage.MetricPrivateNodeHours))
	if bm.Name != "Dedicated Node Hours [vcluster_private_node_hours] by public_ip, zone" {
		t.Errorf("name %q", bm.Name)
	}
	if want := []string{"region", "sku", usage.DimCapacityType, usage.DimPublicIP, usage.DimZone, usage.DimTenantCluster}; len(bm.GroupKeys) != 1 || !slices.Equal(bm.GroupKeys[0], want) {
		t.Errorf("group keys %v, want one compound key %v", bm.GroupKeys, want)
	}
	required := map[string]bool{}
	for _, f := range bm.PropertyFilters {
		required[f.Name] = f.Exists != nil && *f.Exists
	}
	for _, k := range append([]string{"value"}, bm.GroupKeys[0]...) {
		if !required[k] {
			t.Errorf("%s is not required (Metronome needs every group key on the event)", k)
		}
	}
	body, _ := json.Marshal(bm)
	assertMatchesSchema(t, "create_billable_metric", body)

	cpu := BillableMetricFor(lookup(t, usage.MetricCPUCoreHours))
	if cpu.Name != "CPU Core Hours [vcluster_cpu_core_hours] by sku" || !slices.Contains(cpu.GroupKeys[0], "sku") {
		t.Errorf("CPU core hours: %q %v", cpu.Name, cpu.GroupKeys)
	}
	gpu := BillableMetricFor(lookup(t, usage.MetricGPUHours))
	if gpu.Name != "GPU Hours [vcluster_gpu_hours]" || strings.Join(gpu.GroupKeys[0], ",") != "region,sku,capacity_type,billing_mode,tenant_cluster" {
		t.Errorf("a metric PRICING_KEYS does not name changed: %q %v", gpu.Name, gpu.GroupKeys)
	}
	if dims := (&Adapter{}).PriceDims(usage.MetricPrivateNodeHours); !slices.Contains(dims, usage.DimPublicIP) || !slices.Contains(dims, usage.DimZone) {
		t.Errorf("the price check does not split series by the new keys: %v", dims)
	}
}

// Every event carries every pricing key: one it has no value for gets the
// placeholder the billable metric's exists filter accepts.
func TestEveryEventCarriesEveryPricingKey(t *testing.T) {
	pricingKeys(t, "vcluster_private_node_hours:zone|public_ip|instance_type,vcluster_cpu_core_hours:sku")
	a := New(nil, &config.Config{}, kv.Memory(), nil)
	node := usage.Event{Tenant: "tenant-acme", Metric: usage.MetricPrivateNodeHours, Quantity: 1, SKU: "gpu-h100-8", Region: "ap-southeast-2",
		WindowStart: now, WindowEnd: now.Add(time.Minute), ResourceID: "bm-01",
		Dimensions: map[string]string{usage.DimTenantCluster: "c1", usage.DimCapacityType: "on-demand", usage.DimPublicIP: "true"}}
	node.Finalize()
	props := a.ToIngest(&node).Properties
	for k, want := range map[string]string{usage.DimPublicIP: "true", usage.DimZone: placeholder, usage.DimInstanceType: placeholder, "sku": "gpu-h100-8"} {
		if props[k] != want {
			t.Errorf("%s = %q, want %q", k, props[k], want)
		}
	}
	// CPU of a pod whose node is gone has no SKU: the placeholder.
	cpu := usage.Event{Tenant: "tenant-acme", Metric: usage.MetricCPUCoreHours, Quantity: 1, Region: "ap-southeast-2", WindowStart: now, WindowEnd: now.Add(time.Minute),
		Dimensions: map[string]string{usage.DimTenantCluster: "c1", usage.DimCapacityType: "on-demand", usage.DimBillingMode: "shared"}}
	cpu.Finalize()
	if got := a.ToIngest(&cpu).Properties["sku"]; got != placeholder {
		t.Errorf("CPU without a node SKU: sku %q, want %q", got, placeholder)
	}
	// Every property the billable metric requires is there.
	for _, e := range []usage.Event{node, cpu} {
		props := a.ToIngest(&e).Properties
		for _, f := range BillableMetricFor(lookup(t, e.Metric)).PropertyFilters {
			if f.Exists != nil && *f.Exists && props[f.Name] == "" {
				t.Errorf("%s: the billable metric requires %s, which the event lacks", e.Metric, f.Name)
			}
		}
	}
}

// With METRONOME_PROVISION, new pricing keys mean a new billable metric:
// Metronome cannot change one's group keys. vBilling creates it, checks
// prices on it and says what to do with the old one, which it never changes
// or archives. Back to the old keys, it uses the old one again.
func TestProvisionModeCreatesANewBillableMetricWhenThePricingKeysChange(t *testing.T) {
	f := newFake(t)
	logs := captureLog(t)
	provision := func(c *config.Config) { c.MetronomeProvision = true }
	a := newReadOnly(t, f, provision)
	ctx := context.Background()
	bootstrap := func(a *Adapter) {
		t.Helper()
		if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
			t.Fatal(err)
		}
	}
	creates := func() int {
		n := 0
		for _, w := range f.Writes() {
			if w == "POST /v1/billable-metrics/create" {
				n++
			}
			if strings.Contains(w, "archive") {
				t.Errorf("vBilling archived something: %s", w)
			}
		}
		return n
	}
	byID := func(id string) BillableMetric {
		t.Helper()
		bms, err := a.c.ListBillableMetrics(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range bms {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("no billable metric %s", id)
		return BillableMetric{}
	}

	bootstrap(a)
	first, _ := a.state.Get("bm/" + usage.MetricPrivateNodeHours)
	gpuBM, _ := a.state.Get("bm/" + usage.MetricGPUHours)
	base := creates()
	if base != len(usage.Billable()) {
		t.Fatalf("first bootstrap created %d billable metrics, want %d", base, len(usage.Billable()))
	}

	pricingKeys(t, "vcluster_private_node_hours:public_ip|zone")
	bootstrap(a)
	second, _ := a.state.Get("bm/" + usage.MetricPrivateNodeHours)
	if second == first || creates() != base+1 {
		t.Fatalf("new keys: billable metric %s (was %s), %d created", second, first, creates()-base)
	}
	if m := byID(second); m.Name != "Dedicated Node Hours [vcluster_private_node_hours] by public_ip, zone" || !groupsOn(m, GroupKeys(lookup(t, usage.MetricPrivateNodeHours))) {
		t.Errorf("new billable metric: %+v", m)
	}
	if m := byID(first); m.ArchivedAt != "" || slices.Contains(groupedKeys(m), usage.DimPublicIP) {
		t.Errorf("the old billable metric changed: %+v", m)
	}
	if id, _ := a.state.Get("bm/" + usage.MetricGPUHours); id != gpuBM {
		t.Error("a metric PRICING_KEYS does not name moved to another billable metric")
	}
	warning := logs.lines("WARNING", usage.MetricPrivateNodeHours, "Metronome cannot change a billable metric's group keys")
	if len(warning) != 1 || !strings.Contains(warning[0], first) || !strings.Contains(warning[0], second) ||
		!strings.Contains(warning[0], "end the rates of the products on") || !strings.Contains(warning[0], "never changes or archives") {
		t.Fatalf("the change is not explained: %v", warning)
	}

	// Again, after a restart, and after a restart that lost the state: the
	// new billable metric is found, nothing is created.
	bootstrap(a)
	bootstrap(newReadOnly(t, f, provision))
	lost := newReadOnly(t, f, provision)
	bootstrap(lost)
	if id, _ := lost.state.Get("bm/" + usage.MetricPrivateNodeHours); id != second || creates() != base+1 {
		t.Errorf("restart with lost state: %s, %d created", id, creates()-base)
	}

	// Back to the metric's own keys: the first billable metric again.
	usage.SetPricingKeys(nil)
	bootstrap(a)
	if id, _ := a.state.Get("bm/" + usage.MetricPrivateNodeHours); id != first || creates() != base+1 {
		t.Errorf("keys cleared: billable metric %s, want %s again (%d created)", id, first, creates()-base)
	}
}

// A billable metric that has vBilling's name but other group keys (Finance
// made it by hand) is never adopted: vBilling creates its own under a name
// made distinct by its keys, and says that both count the usage.
func TestProvisionModeDoesNotAdoptABillableMetricWithOtherGroupKeys(t *testing.T) {
	f := newFake(t)
	logs := captureLog(t)
	theirs := financeMetric(t, f, usage.MetricPublicIPHours, "Public IP Hours [vcluster_public_ip_hours]", func(m *BillableMetric) {
		m.GroupKeys = [][]string{{"region"}}
	})
	a := newReadOnly(t, f, func(c *config.Config) { c.MetronomeProvision = true })
	if err := a.Bootstrap(context.Background(), usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	ours, _ := a.state.Get("bm/" + usage.MetricPublicIPHours)
	if ours == "" || ours == theirs {
		t.Fatalf("adopted the billable metric with other group keys: %s", ours)
	}
	bms, _ := a.c.ListBillableMetrics(context.Background())
	for _, m := range bms {
		if m.ID == ours && !strings.HasPrefix(m.Name, "Public IP Hours [vcluster_public_ip_hours] #") {
			t.Errorf("name %q is not distinct", m.Name)
		}
	}
	if len(logs.lines("WARNING", theirs, ours)) != 1 {
		t.Error("the log does not say that both billable metrics count the usage")
	}
}

// The price check reads every combination of the pricing keys as its own
// series: on a rate card that prices a profile with a public IP at 5 and
// without one at 0, the two are two rows, priced and zero, and public IP
// hours have their own row.
func TestPriceCheckReadsEveryCombinationOfThePricingKeys(t *testing.T) {
	pricingKeys(t, "vcluster_private_node_hours:public_ip")
	f := newFake(t)
	nodes := financeMetric(t, f, usage.MetricPrivateNodeHours, "Instance hours by public IP")
	ips := financeMetric(t, f, usage.MetricPublicIPHours, "Public IP hours")
	instance := f.AddProduct("Instance hours", nodes, "sku", usage.DimPublicIP)
	publicIP := f.AddProduct("Public IP", ips, "region")
	card := f.AddRateCard("AI Cloud list")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: instance, Price: 500, PricingGroupValues: map[string]string{"sku": "gpu-h100-8", usage.DimPublicIP: "true"}})
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: instance, Price: 0, PricingGroupValues: map[string]string{"sku": "gpu-h100-8", usage.DimPublicIP: "false"}})
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: publicIP, Price: 1, PricingGroupValues: map[string]string{"region": "ap-southeast-2"}})
	cus, _ := f.CreateCustomer("Acme AI", "org_1", "tenant-acme")
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: now.AddDate(0, -1, 0)})

	a := newReadOnly(t, f)
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	svc := pricing.New(sp, []destinations.Destination{a}, nil, nil, nil, nil, pricing.Options{Mode: pricing.ModeWarn})
	event := func(metric, node, publicIP string) usage.Event {
		e := usage.Event{Tenant: "tenant-acme", Metric: metric, Quantity: 1, Region: "ap-southeast-2", ResourceID: node,
			WindowStart: now.Add(-time.Minute), WindowEnd: now,
			Dimensions: map[string]string{usage.DimTenantCluster: "c1", usage.DimCapacityType: "on-demand", usage.DimBillingMode: "private_node", usage.DimNode: node}}
		if metric == usage.MetricPrivateNodeHours {
			e.SKU = "gpu-h100-8"
			e.Dimensions[usage.DimPublicIP] = publicIP
		} else {
			e.Dimensions[usage.DimIPFamily] = "ipv4"
		}
		e.Finalize()
		return e
	}
	svc.Observe([]usage.Event{event(usage.MetricPrivateNodeHours, "bm-01", "true"), event(usage.MetricPrivateNodeHours, "bm-02", "false"),
		event(usage.MetricPublicIPHours, "bm-01", "")}, time.Now())
	rep := svc.CheckNow(context.Background())
	rep = svc.CheckNow(context.Background()) // the tenant's series move to its contract's rate card

	rows := map[string]pricing.Row{}
	for _, d := range rep.Destinations {
		for _, r := range d.Series {
			if !r.NoUsageYet {
				rows[r.Metric+"/"+r.Dims[usage.DimPublicIP]] = r
			}
		}
	}
	if len(rows) != 3 {
		t.Fatalf("rows %v, want one per combination", rows)
	}
	if r := rows[usage.MetricPrivateNodeHours+"/true"]; r.Status != destinations.PriceCovered || r.Plan != card || !strings.Contains(r.Detail, "public_ip=true") {
		t.Errorf("with a public IP: %+v", r)
	}
	if r := rows[usage.MetricPrivateNodeHours+"/false"]; r.Status != destinations.PriceZero || !strings.Contains(r.Detail, "public_ip=false") {
		t.Errorf("without a public IP, priced at 0 on purpose, is its own row: %+v", r)
	}
	if r := rows[usage.MetricPublicIPHours+"/"]; r.Status != destinations.PriceCovered {
		t.Errorf("public IP hours: %+v", r)
	}
	for _, r := range rows {
		if r.Note != "" {
			t.Errorf("%s: a billable metric that groups on every pricing key needs no note: %s", r.Metric, r.Note)
		}
	}
}

// Without METRONOME_PROVISION, a billable metric Finance made before the
// keys were added cannot price on them: start-up says so, and so does the
// price check, next to its answer.
func TestDefaultModeSaysWhenNoBillableMetricGroupsOnThePricingKeys(t *testing.T) {
	f := newFake(t)
	old := financeMetric(t, f, usage.MetricGPUHours, "GPU hours") // made with the keys of the time
	prod := f.AddProduct("GPU hours", old, "sku")
	card := f.AddRateCard("List")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: prod, Price: 249, PricingGroupValues: map[string]string{"sku": "gpu-h100-8"}})
	cus, _ := f.CreateCustomer("Acme AI", "tenant-acme")
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: now.AddDate(0, -1, 0)})

	pricingKeys(t, "vcluster_gpu_hours:gpu_type")
	logs := captureLog(t)
	a := newReadOnly(t, f)
	if err := a.Bootstrap(context.Background(), usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if w := logs.lines("WARNING", "PRICING_KEYS prices vcluster_gpu_hours on gpu_type", "GPU Hours [vcluster_gpu_hours] by gpu_type"); len(w) != 1 {
		t.Errorf("start-up does not say the billable metric lacks the key: %v", logs.lines("vcluster_gpu_hours"))
	}
	ev := usage.Event{Tenant: "tenant-acme", Metric: usage.MetricGPUHours, SKU: "gpu-h100-8", Region: "ap-southeast-2",
		Dimensions: map[string]string{usage.DimCapacityType: "on-demand", usage.DimBillingMode: "shared", usage.DimGPUType: "NVIDIA-H100-80GB-HBM3"}}
	c := coverageOf(t, a, destinations.SeriesOf(a, a.PricePlan(usage.Tenant{ID: "tenant-acme"}), &ev))[0]
	if c.Status != destinations.PriceCovered || !strings.Contains(c.Note, `billable metric "GPU hours" does not group on it`) || !strings.Contains(c.Note, "gpu_type") {
		t.Errorf("%+v", c)
	}
	// Once Finance adds the billable metric vBilling prints, the note goes.
	financeMetric(t, f, usage.MetricGPUHours, "GPU Hours [vcluster_gpu_hours] by gpu_type")
	if c := coverageOf(t, a, destinations.SeriesOf(a, a.PricePlan(usage.Tenant{ID: "tenant-acme"}), &ev))[0]; strings.Contains(c.Note, "does not group on") {
		t.Errorf("note after the new billable metric: %q", c.Note)
	}
}
