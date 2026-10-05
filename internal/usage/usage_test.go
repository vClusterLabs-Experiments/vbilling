package usage

import (
	"math"
	"strings"
	"testing"
	"time"
)

func baseEvent() Event {
	ws := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	return Event{
		Tenant:      "acme",
		Metric:      MetricGPUHours,
		Quantity:    8.0 / 60,
		WindowStart: ws,
		WindowEnd:   ws.Add(time.Minute),
		Region:      "ap-southeast-2",
		SKU:         "NVIDIA-H100-80GB-HBM3",
		ResourceID:  "vcluster-team-a-gpu",
		Dimensions:  map[string]string{DimGPUType: "NVIDIA-H100-80GB-HBM3", DimCapacityType: CapacityOnDemand},
	}
}

func TestDeriveIDIsDeterministicAndIgnoresQuantity(t *testing.T) {
	a, b := baseEvent(), baseEvent()
	b.Quantity = 99
	b.Properties = map[string]any{"noise": true}
	if DeriveID(&a) != DeriveID(&b) {
		t.Fatal("quantity/properties must not change the ID")
	}
	if !strings.HasPrefix(DeriveID(&a), "vb1_") || len(DeriveID(&a)) != 36 {
		t.Fatalf("unexpected ID shape %q", DeriveID(&a))
	}
}

func TestDeriveIDChangesWithIdentity(t *testing.T) {
	base := baseEvent()
	id := DeriveID(&base)
	mutations := map[string]func(*Event){
		"tenant": func(e *Event) { e.Tenant = "other" },
		"metric": func(e *Event) { e.Metric = MetricCPUCoreHours },
		"window": func(e *Event) {
			e.WindowStart = e.WindowStart.Add(time.Minute)
			e.WindowEnd = e.WindowEnd.Add(time.Minute)
		},
		"region":    func(e *Event) { e.Region = "ap-southeast-4" },
		"sku":       func(e *Event) { e.SKU = "other" },
		"resource":  func(e *Event) { e.ResourceID = "other" },
		"dimension": func(e *Event) { e.Dimensions[DimCapacityType] = CapacitySpot },
		"new dim":   func(e *Event) { e.Dimensions["extra"] = "x" },
	}
	for name, mutate := range mutations {
		e := baseEvent()
		mutate(&e)
		if DeriveID(&e) == id {
			t.Errorf("%s change did not change the ID", name)
		}
	}
}

func TestDeriveIDIsTimezoneIndependent(t *testing.T) {
	a := baseEvent()
	b := baseEvent()
	syd := time.FixedZone("AEST", 10*3600)
	b.WindowStart = b.WindowStart.In(syd)
	b.WindowEnd = b.WindowEnd.In(syd)
	if DeriveID(&a) != DeriveID(&b) {
		t.Fatal("same instant in another zone must produce the same ID")
	}
}

func TestFinalizeAndValidate(t *testing.T) {
	e := baseEvent()
	e.Finalize()
	if e.SchemaVersion != SchemaVersion || e.Unit != "gpu-hours" || e.ID == "" {
		t.Fatalf("finalize did not fill fields: %+v", e)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	bad := baseEvent()
	bad.Finalize()
	bad.Quantity = math.NaN()
	bad.Metric = "Bad-Metric"
	bad.WindowEnd = bad.WindowStart
	bad.Dimensions["Bad Key"] = "x"
	err := bad.Validate()
	if err == nil {
		t.Fatal("invalid event accepted")
	}
	for _, want := range []string{"quantity", "metric", "window_end", "dimension key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestWindowAlignment(t *testing.T) {
	ts := time.Date(2026, 10, 5, 10, 7, 42, 500, time.UTC)
	ws, we := Window(ts, 5*time.Minute)
	if !ws.Equal(time.Date(2026, 10, 5, 10, 5, 0, 0, time.UTC)) || !we.Equal(ws.Add(5*time.Minute)) {
		t.Fatalf("got [%s, %s)", ws, we)
	}
}

func TestOverlap(t *testing.T) {
	ws := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	we := ws.Add(time.Minute)
	cases := []struct {
		name       string
		start, end time.Time
		want       time.Duration
	}{
		{"open interval started before", ws.Add(-time.Hour), time.Time{}, time.Minute},
		{"started mid window", ws.Add(20 * time.Second), time.Time{}, 40 * time.Second},
		{"ended mid window", ws.Add(-time.Hour), ws.Add(15 * time.Second), 15 * time.Second},
		{"inside window", ws.Add(10 * time.Second), ws.Add(25 * time.Second), 15 * time.Second},
		{"ended before window", ws.Add(-time.Hour), ws.Add(-time.Second), 0},
		{"starts after window", we.Add(time.Second), time.Time{}, 0},
	}
	for _, c := range cases {
		if got := Overlap(c.start, c.end, ws, we); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestCustomMetrics(t *testing.T) {
	defer resetCustomForTest()
	defs, err := ParseCustomMetrics("inference_output_tokens:tokens:model|region, slurm_gpu_hours:gpu-hours")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if err := RegisterCustom(d); err != nil {
			t.Fatal(err)
		}
	}
	d, ok := Lookup("inference_output_tokens")
	if !ok || d.Unit != "tokens" || !d.Billable || !d.Custom || len(d.GroupKeys) != 2 {
		t.Fatalf("unexpected custom def %+v", d)
	}
	if err := RegisterCustom(MetricDef{Code: MetricGPUHours}); err == nil {
		t.Fatal("expected collision with builtin metric")
	}
	if err := RegisterCustom(MetricDef{Code: "Bad Code"}); err == nil {
		t.Fatal("expected invalid code error")
	}
	found := false
	for _, c := range Billable() {
		if c.Code == "slurm_gpu_hours" {
			found = true
		}
		if c.Code == MetricGPUUtilization {
			t.Error("informational metric listed as billable")
		}
	}
	if !found {
		t.Error("custom metric missing from Billable()")
	}
}
