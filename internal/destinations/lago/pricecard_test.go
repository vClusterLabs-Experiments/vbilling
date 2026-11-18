package lago

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeCard(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "price-card.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPriceCard(t *testing.T) {
	card, err := LoadPriceCard(writeCard(t, `
vcluster_gpu_hours:
  amount: "0"
  sku:
    NVIDIA-H100-80GB-HBM3: "2.49"
    NVIDIA-L4: 0.8
    NVIDIA-A100: 1.23456789
inference_output_tokens:
  amount: 0.000002
vcluster_cpu_core_hours:
  amount: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	want := PriceCard{
		"vcluster_gpu_hours":      {Amount: "0", SKU: map[string]Amount{"NVIDIA-H100-80GB-HBM3": "2.49", "NVIDIA-L4": "0.8", "NVIDIA-A100": "1.23456789"}},
		"inference_output_tokens": {Amount: "0.000002"},
		"vcluster_cpu_core_hours": {Amount: "1"},
	}
	if !reflect.DeepEqual(card, want) {
		t.Fatalf("card = %+v\nwant   %+v", card, want)
	}
	if card.chargeAmount("vcluster_memory_gb_hours") != "0" || card.chargeAmount("inference_output_tokens") != "0.000002" {
		t.Fatal("chargeAmount")
	}
	if a, ok := card.skuAmount("vcluster_gpu_hours", "NVIDIA-H100-80GB-HBM3"); !ok || a != "2.49" {
		t.Fatal("skuAmount")
	}
	if _, ok := card.skuAmount("vcluster_gpu_hours", "NVIDIA-B200"); ok {
		t.Fatal("unlisted SKU has no price card amount")
	}
	if got := card.skus(); !reflect.DeepEqual(got, map[string][]string{"vcluster_gpu_hours": {"NVIDIA-A100", "NVIDIA-H100-80GB-HBM3", "NVIDIA-L4"}}) {
		t.Fatalf("skus = %v", got)
	}

	// Helm's toYaml writes an unquoted 0.000002 as 2e-06.
	if card, err := LoadPriceCard(writeCard(t, "m:\n  amount: 2e-06\n")); err != nil || card["m"].Amount != "0.000002" {
		t.Fatalf("2e-06: %v %v", card, err)
	}
	if card, err := LoadPriceCard(""); err != nil || card != nil {
		t.Fatal("no price card")
	}
	for name, bad := range map[string]string{
		"negative":    "m:\n  amount: \"-1\"\n",
		"not decimal": "m:\n  amount: \"2,49\"\n",
		"typo":        "m:\n  amount: \"1\"\n  skus:\n    a: \"1\"\n",
		"word":        "m:\n  sku:\n    a: free\n",
	} {
		if _, err := LoadPriceCard(writeCard(t, bad)); err == nil {
			t.Errorf("%s: want an error", name)
		} else if !strings.Contains(err.Error(), "price-card.yaml") {
			t.Errorf("%s: error should name the file: %v", name, err)
		}
	}
}
