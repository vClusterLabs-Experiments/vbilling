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
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

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
	Currency string                          `json:"currency,omitempty"`
	Interval string                          `json:"interval,omitempty"`
	Plans    map[string]map[string]CardPrice `json:"plans"`
}

// CardPrice is one metric's prices in a plan: Amount for usage without a
// SKU, SKU for usage per SKU (each its own meter, see splitMetersBy).
type CardPrice struct {
	Amount CardAmount            `json:"amount,omitempty"`
	Name   string                `json:"name,omitempty"`
	Per    int64                 `json:"per,omitempty"`
	SKU    map[string]CardAmount `json:"sku,omitempty"`
	Names  map[string]string     `json:"names,omitempty"`
}

// CardAmount is a non-negative decimal amount. YAML numbers are accepted;
// quoting keeps every digit.
type CardAmount string

func (a *CardAmount) UnmarshalJSON(b []byte) error {
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
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 || strings.ContainsAny(s, "eE/") {
		return fmt.Errorf("amount %q must be a non-negative decimal such as \"2.49\"", s)
	}
	*a = CardAmount(s)
	return nil
}

// zeroDecimal lists Stripe's currencies without minor units.
var zeroDecimal = map[string]bool{"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true, "krw": true,
	"mga": true, "pyg": true, "rwf": true, "ugx": true, "vnd": true, "vuv": true, "xaf": true, "xof": true, "xpf": true}

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
	if !zeroDecimal[strings.ToLower(currency)] {
		r.Mul(r, big.NewRat(100, 1))
	}
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
