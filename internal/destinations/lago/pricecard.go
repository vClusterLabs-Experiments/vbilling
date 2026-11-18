package lago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"

	"sigs.k8s.io/yaml"
)

// PriceCard is the optional LAGO_PRICE_CARD file: amounts per metric code,
// and per SKU, that vBilling uses when it creates a charge or a charge
// filter. vBilling never changes an amount that already exists in Lago, so
// prices edited in the Lago UI stay as they are.
//
//	vcluster_gpu_hours:
//	  amount: "0"
//	  sku:
//	    NVIDIA-H100-80GB-HBM3: "2.49"
//	inference_output_tokens:
//	  amount: "0.000002"
type PriceCard map[string]MetricPrice

// MetricPrice is one metric's entry in the price card.
type MetricPrice struct {
	Amount Amount            `json:"amount,omitempty"` // the charge's amount per unit
	SKU    map[string]Amount `json:"sku,omitempty"`    // charge filter amounts per SKU
}

// Amount is a non-negative decimal amount per unit. Lago only accepts
// amounts as strings; YAML numbers are accepted and converted, but quoting
// keeps every digit.
type Amount string

var decimalRe = regexp.MustCompile(`^([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)

func (a *Amount) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	} else {
		f, err := strconv.ParseFloat(string(b), 64)
		if err != nil {
			return fmt.Errorf("amount %s is not a number", b)
		}
		s = strconv.FormatFloat(f, 'f', -1, 64)
	}
	if !decimalRe.MatchString(s) {
		return fmt.Errorf("amount %q must be a non-negative decimal such as \"2.49\"", s)
	}
	*a = Amount(s)
	return nil
}

// LoadPriceCard reads a price card. An empty path means no price card.
func LoadPriceCard(path string) (PriceCard, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Convert without a typed target: yaml.Unmarshal would turn unquoted
	// amounts into strings with float32 precision ("2e-06", "1.2345679").
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var card PriceCard
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&card); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return card, nil
}

// chargeAmount is the amount of a charge vBilling creates for metric.
func (p PriceCard) chargeAmount(metric string) string {
	if a := p[metric].Amount; a != "" {
		return string(a)
	}
	return "0"
}

// skuAmount is the price card amount for a new charge filter on sku.
func (p PriceCard) skuAmount(metric, sku string) (string, bool) {
	a, ok := p[metric].SKU[sku]
	return string(a), ok
}

// skus lists the SKUs the price card prices, per metric.
func (p PriceCard) skus() map[string][]string {
	out := map[string][]string{}
	for metric, e := range p {
		for sku := range e.SKU {
			out[metric] = append(out[metric], sku)
		}
		sort.Strings(out[metric])
	}
	return out
}
