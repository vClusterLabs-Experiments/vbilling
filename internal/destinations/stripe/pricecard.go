package stripe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// PriceCard is the optional STRIPE_PRICE_CARD file: the metered prices of
// each plan, which vBilling creates in Stripe when they are missing, with
// their meters. It never changes or archives a price: Stripe prices cannot
// be edited, so a new amount is a new price that Finance puts in place.
// Amounts are in the currency's main unit (dollars, not cents) per unit,
// or per `per` units: token prices are quoted per million tokens.
//
//	currency: usd       # default: billing.currency
//	interval: month     # day, week, month or year (default month)
//	plans:
//	  dedicated-a100:
//	    vcluster_gpu_hours:
//	      sku: {NVIDIA-A100-SXM4-40GB: "2.20"}
//	      names: {NVIDIA-A100-SXM4-40GB: "A100 40GB GPU-hour"}
//	    vcluster_instance_hours: {amount: "0.10", name: "Cluster control plane hour"}
//	  serverless:
//	    inference_output_tokens:
//	      per: 1000000
//	      sku: {qwen2.5-7b-instruct: "0.60"}
type PriceCard struct {
	Currency string                          `json:"currency,omitempty" yaml:"currency,omitempty"`
	Interval string                          `json:"interval,omitempty" yaml:"interval,omitempty"`
	Plans    map[string]map[string]CardPrice `json:"plans" yaml:"plans"`
}

// CardPrice is one metric's prices in a plan: Amount for usage without a
// SKU, SKU for usage per SKU (each its own meter, see splitMetersBy).
type CardPrice struct {
	Amount CardAmount            `json:"amount,omitempty" yaml:"amount,omitempty"`
	Name   string                `json:"name,omitempty" yaml:"name,omitempty"`
	Per    int64                 `json:"per,omitempty" yaml:"per,omitempty"`
	SKU    map[string]CardAmount `json:"sku,omitempty" yaml:"sku,omitempty"`
	Names  map[string]string     `json:"names,omitempty" yaml:"names,omitempty"`
}

// CardAmount is a non-negative decimal amount, kept as written: a YAML or
// JSON number is read as its literal text, never through a float, so
// 2.4999999 stays 2.4999999.
type CardAmount string

var decimalAmount = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func parseCardAmount(s string) (CardAmount, error) {
	s = strings.TrimSpace(s)
	if !decimalAmount.MatchString(s) {
		return "", fmt.Errorf("amount %q must be a non-negative decimal such as 2.49 (no exponent, sign or separators)", s)
	}
	return CardAmount(s), nil
}

func (a *CardAmount) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	s := string(b) // a JSON number: its literal text
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	v, err := parseCardAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// UnmarshalYAML reads a scalar's literal text: a YAML number decoded into a
// string keeps every digit it was written with.
func (a *CardAmount) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	v, err := parseCardAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// zeroDecimal lists Stripe's currencies without minor units. UGX and ISK
// are not here although they have no fractions: Stripe's currencies page
// ("Special cases", https://docs.stripe.com/currencies) says backward
// compatibility requires them as two-decimal values ending in 00, so
// 5 UGX is sent as 500.
var zeroDecimal = map[string]bool{"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true, "krw": true,
	"mga": true, "pyg": true, "rwf": true, "vnd": true, "vuv": true, "xaf": true, "xof": true, "xpf": true}

// threeDecimal lists Stripe's currencies with 1000 minor units per main
// unit. Stripe's support article on the UAE says "We also support charges
// in the following three-decimal currencies" and names exactly these:
// https://support.stripe.com/questions/which-payments-methods-and-products-are-available-in-the-uae
// https://docs.stripe.com/currencies lists them as presentment currencies;
// as of 2026-11-19 that page no longer carries its three-decimal section
// (#three-decimal), only its description still mentions "three-decimal
// currency support". That section asked for charge amounts ending in 0 (a
// multiple of 10 minor units). unit_amount_decimal is not a charge: it is
// a price per unit, up to 12 decimals of a minor unit, which Stripe
// multiplies by the metered quantity and rounds when it invoices. So the
// card's amount is converted exactly and never rounded here; if Stripe
// refused one, creating the price would fail at startup, loudly, instead
// of billing another price.
var threeDecimal = map[string]bool{"bhd": true, "jod": true, "kwd": true, "omr": true, "tnd": true}

// minorUnits is how many minor units make one main unit of a currency.
func minorUnits(currency string) int64 {
	switch c := strings.ToLower(currency); {
	case zeroDecimal[c]:
		return 1
	case threeDecimal[c]:
		return 1000
	}
	return 100
}

// unitAmountDecimal converts an amount in main units per `per` units to
// Stripe's unit_amount_decimal: minor units per unit, at most 12 decimals.
func unitAmountDecimal(amount CardAmount, per int64, currency string) (string, error) {
	r, ok := new(big.Rat).SetString(string(amount))
	if !ok {
		return "", fmt.Errorf("amount %q is not a decimal", amount)
	}
	if per <= 0 {
		per = 1
	}
	r.Quo(r, new(big.Rat).SetInt64(per))
	r.Mul(r, big.NewRat(minorUnits(currency), 1))
	s := r.FloatString(12)
	back, _ := new(big.Rat).SetString(s)
	if back.Cmp(r) != 0 {
		return "", fmt.Errorf("amount %s per %d is finer than Stripe's 12 decimal places of a minor unit", amount, per)
	}
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" {
		s = "0"
	}
	return s, nil
}

// LoadPriceCard reads and checks STRIPE_PRICE_CARD.
func LoadPriceCard(path, defaultCurrency string) (*PriceCard, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Decoded from YAML directly, not through JSON: sigs.k8s.io/yaml turns an
	// unquoted amount into a string with float32 precision (1.23456789 became
	// 1.2345679) before CardAmount saw it.
	var pc PriceCard
	if err := yaml.UnmarshalStrict(b, &pc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if pc.Currency == "" {
		pc.Currency = defaultCurrency
	}
	pc.Currency = strings.ToLower(pc.Currency)
	if pc.Currency == "" {
		return nil, fmt.Errorf("%s: no currency (set currency or billing.currency)", path)
	}
	if pc.Interval == "" {
		pc.Interval = "month"
	}
	switch pc.Interval {
	case "day", "week", "month", "year":
	default:
		return nil, fmt.Errorf("%s: interval %q must be day, week, month or year", path, pc.Interval)
	}
	if len(pc.Plans) == 0 {
		return nil, fmt.Errorf("%s: no plans", path)
	}
	for plan, metrics := range pc.Plans {
		for metric, cp := range metrics {
			if cp.Per < 0 {
				return nil, fmt.Errorf("%s: %s/%s: per must be positive", path, plan, metric)
			}
			if cp.Amount == "" && len(cp.SKU) == 0 {
				return nil, fmt.Errorf("%s: %s/%s: needs an amount or sku amounts", path, plan, metric)
			}
			for _, e := range cp.entries() {
				if _, err := unitAmountDecimal(e.amount, cp.Per, pc.Currency); err != nil {
					return nil, fmt.Errorf("%s: %s/%s: %w", path, plan, metric, err)
				}
			}
		}
	}
	return &pc, nil
}

type cardEntry struct {
	sku    string
	amount CardAmount
	name   string
}

func (cp CardPrice) entries() []cardEntry {
	var out []cardEntry
	if cp.Amount != "" {
		out = append(out, cardEntry{amount: cp.Amount, name: cp.Name})
	}
	skus := make([]string, 0, len(cp.SKU))
	for s := range cp.SKU {
		skus = append(skus, s)
	}
	sort.Strings(skus)
	for _, s := range skus {
		out = append(out, cardEntry{sku: s, amount: cp.SKU[s], name: cp.Names[s]})
	}
	return out
}

// CreatePrice creates a metered price on a meter.
func (c *Client) CreatePrice(ctx context.Context, f url.Values, idemKey string) (Price, error) {
	var p Price
	err := c.do(ctx, http.MethodPost, "/v1/prices", f, idemKey, &p)
	return p, err
}

// applyPriceCard creates the price card's missing meters and prices.
func (a *Adapter) applyPriceCard(ctx context.Context) error {
	pc := a.card
	if pc == nil {
		return nil
	}
	existing, err := a.c.ListMeteredPrices(ctx)
	if err != nil {
		return fmt.Errorf("list prices: %w", err)
	}
	plans := make([]string, 0, len(pc.Plans))
	for p := range pc.Plans {
		plans = append(plans, p)
	}
	sort.Strings(plans)
	var errs []error
	created := 0
	for _, plan := range plans {
		metrics := make([]string, 0, len(pc.Plans[plan]))
		for m := range pc.Plans[plan] {
			metrics = append(metrics, m)
		}
		sort.Strings(metrics)
		for _, metric := range metrics {
			cp := pc.Plans[plan][metric]
			if _, ok := usage.Lookup(metric); !ok {
				errs = append(errs, fmt.Errorf("price card %s: unknown metric %q", plan, metric))
				continue
			}
			for _, e := range cp.entries() {
				ok, err := a.applyCardPrice(ctx, existing, pc, plan, metric, cp.Per, e)
				if err != nil {
					errs = append(errs, err)
				}
				if ok {
					created++
				}
			}
		}
	}
	if created > 0 {
		a.priceMu.Lock()
		a.pricesTime = time.Time{} // new prices reach subscriptions on the next ensure
		a.priceMu.Unlock()
		log.Printf("[stripe] price card: created %d price(s)", created)
	}
	return errors.Join(errs...)
}

func (a *Adapter) applyCardPrice(ctx context.Context, existing []Price, pc *PriceCard, plan, metric string, per int64, e cardEntry) (bool, error) {
	ev := usage.Event{Metric: metric, SKU: e.sku, Dimensions: map[string]string{}}
	name := a.MeterName(&ev)
	m, err := a.ensureMeter(ctx, name, meterDisplayName(&ev, name))
	if err != nil {
		return false, err
	}
	want, _ := unitAmountDecimal(e.amount, per, pc.Currency)
	for _, p := range existing {
		if !p.Active || p.Recurring == nil || p.Recurring.Meter != m.ID || p.Metadata[metaPlan] != plan || !strings.EqualFold(p.Currency, pc.Currency) {
			continue
		}
		if have := priceDecimal(p); have != want {
			log.Printf("[stripe] price card: plan %s meter %s has price %s at %s, the card says %s; vBilling never changes prices (create the new price, archive the old one)",
				plan, name, p.ID, have, want)
		}
		return false, nil
	}
	product := e.name
	if product == "" {
		product = meterDisplayName(&ev, name)
	}
	f := url.Values{}
	f.Set("currency", pc.Currency)
	f.Set("unit_amount_decimal", want)
	f.Set("recurring[interval]", pc.Interval)
	f.Set("recurring[usage_type]", "metered")
	f.Set("recurring[meter]", m.ID)
	f.Set("product_data[name]", truncate(product, 250))
	f.Set("nickname", truncate(product, 250))
	f.Set("metadata["+metaPlan+"]", plan)
	f.Set("metadata[vbilling_meter]", name)
	f.Set("metadata[vbilling_price_card]", "true")
	key := "vbilling-pc-" + shortHash(strings.Join([]string{plan, m.ID, pc.Currency, pc.Interval, want}, "|"))
	p, err := a.c.CreatePrice(ctx, f, key)
	if err != nil {
		return false, fmt.Errorf("price card %s: create price on %s: %w", plan, name, err)
	}
	log.Printf("[stripe] price card: plan %s, %s: %s minor units of %s per unit (%s)", plan, name, want, strings.ToUpper(pc.Currency), p.ID)
	return true, nil
}

// priceDecimal is a price's amount in minor units as unitAmountDecimal
// writes it.
func priceDecimal(p Price) string {
	s := ""
	switch {
	case p.UnitAmountDecimal != nil:
		s = *p.UnitAmountDecimal
	case p.UnitAmount != nil:
		s = strconv.FormatInt(*p.UnitAmount, 10)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return s
	}
	out := strings.TrimRight(strings.TrimRight(r.FloatString(12), "0"), ".")
	if out == "" {
		out = "0"
	}
	return out
}
