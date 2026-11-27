package usage

import (
	"strings"
	"testing"
)

func defIn(defs []MetricDef, code string) (MetricDef, bool) {
	for _, d := range defs {
		if d.Code == code {
			return d, true
		}
	}
	return MetricDef{}, false
}

// With storage allowances, billing platforms get the storage above them
// (vcluster_storage_overage_gb_hours) and not the whole capacity, which
// stays in the catalog as an informational metric. Without them, nothing
// changes: the overage metric is not in the catalog.
func TestStorageAllowancesSwitchTheBilledStorageMetric(t *testing.T) {
	t.Cleanup(func() { UseStorageAllowances(false) })

	if _, ok := defIn(Catalog(), MetricStorageOverageGBHours); ok {
		t.Fatal("the overage metric is in the catalog without an allowance")
	}
	if d, _ := Lookup(MetricStorageGBHours); !d.Billable {
		t.Fatal("storage must be billable without an allowance")
	}
	over, ok := Lookup(MetricStorageOverageGBHours)
	if !ok || !over.Billable || over.Unit != "gib-hours" || over.Name != "Storage GiB Hours above allowance" ||
		strings.Join(over.GroupKeys, ",") != "region,sku" || over.Accuracy != AccuracyExact || over.Source != SourceKubernetes {
		t.Fatalf("overage metric = %+v, want a billable, exact gib-hours metric priced on region and sku like storage", over)
	}

	UseStorageAllowances(true)
	if _, ok := defIn(Billable(), MetricStorageOverageGBHours); !ok {
		t.Error("with an allowance, the overage metric is billable")
	}
	if _, ok := defIn(Billable(), MetricStorageGBHours); ok {
		t.Error("with an allowance, storage is informational")
	}
	if d, ok := defIn(Catalog(), MetricStorageGBHours); !ok || d.Billable {
		t.Errorf("storage stays in the catalog, informational: %+v %v", d, ok)
	}
	if d, _ := Lookup(MetricStorageGBHours); d.Billable {
		t.Error("Lookup must report storage informational: billing platforms skip it by Lookup")
	}

	UseStorageAllowances(false)
	if _, ok := defIn(Catalog(), MetricStorageOverageGBHours); ok {
		t.Error("the overage metric must leave the catalog again")
	}
	if _, ok := defIn(Billable(), MetricStorageGBHours); !ok {
		t.Error("storage must be billable again")
	}
}
