package stripe

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReview2PriceCardThreeDecimalCurrencies: Stripe's unit_amount_decimal
// is in the currency's minor unit. unitAmountDecimal multiplies by 100 for
// every currency outside the zero-decimal list, but Stripe's three-decimal
// currencies (BHD, JOD, KWD, OMR, TND) have 1000 minor units per main unit:
// a price card amount of 1.234 KWD per GPU-hour is created as 123.4 fils,
// a tenth of the price.
func TestReview2PriceCardThreeDecimalCurrencies(t *testing.T) {
	for _, cur := range []string{"bhd", "jod", "kwd", "omr", "tnd"} {
		got, err := unitAmountDecimal("1.234", 1, cur)
		if err != nil {
			t.Fatal(err)
		}
		if got != "1234" {
			t.Errorf("%s: 1.234 per unit created as unit_amount_decimal %s, want 1234 (three-decimal currency)", cur, got)
		}
	}
}

// TestReview2PriceCardKeepsUnquotedAmounts: CardAmount says "YAML numbers
// are accepted; quoting keeps every digit". LoadPriceCard decodes with
// sigs.k8s.io/yaml into a typed target, which turns an unquoted number into
// a string with float32 precision before CardAmount sees it (the Lago price
// card avoids exactly this). 2.4999999 becomes "2.5" and 1.23456789 becomes
// "1.2345679", and the price is created at that amount without a warning.
func TestReview2PriceCardKeepsUnquotedAmounts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "card.yaml")
	card := "currency: usd\nplans:\n  p:\n    vcluster_gpu_hours:\n      amount: 2.4999999\n    vcluster_cpu_core_hours:\n      amount: 1.23456789\n"
	if err := os.WriteFile(p, []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	pc, err := LoadPriceCard(p, "usd")
	if err != nil {
		t.Fatalf("an unquoted amount must be kept or refused, not changed: %v", err)
	}
	for metric, want := range map[string]CardAmount{"vcluster_gpu_hours": "2.4999999", "vcluster_cpu_core_hours": "1.23456789"} {
		if got := pc.Plans["p"][metric].Amount; got != want {
			t.Errorf("%s: unquoted amount %s loaded as %s", metric, want, got)
		}
	}
}
