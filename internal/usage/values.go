package usage

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxValueLen caps a SKU or a dimension value, in bytes. GPU products
// (NVIDIA-H100-80GB-HBM3), model names with a provider prefix and node
// names (at most 253 characters) all fit.
const MaxValueLen = 256

// unsafeValueChars have a meaning in HTML, attributes, scripts or CSV
// formulas. SKUs and dimension values reach dashboards, exports and billing
// platforms' own pages, and some of them are chosen by tenants, so they
// never contain these characters.
const unsafeValueChars = "<>\"'`&"

// CheckValue reports why v cannot be a SKU or a dimension value: it is
// longer than MaxValueLen bytes, is not UTF-8, or contains a control or
// formatting character or one of < > " ' ` &.
func CheckValue(v string) error {
	if len(v) > MaxValueLen {
		return fmt.Errorf("is longer than %d bytes", MaxValueLen)
	}
	if !utf8.ValidString(v) {
		return errors.New("is not valid UTF-8")
	}
	for _, r := range v {
		switch {
		case strings.ContainsRune(unsafeValueChars, r):
			return fmt.Errorf("contains %q (none of %s are allowed)", r, unsafeValueChars)
		case badRune(r):
			return fmt.Errorf("contains the control, formatting or replacement character %U", r)
		}
	}
	return nil
}

// SafeValue returns v with every character CheckValue refuses replaced by
// "_", cut to MaxValueLen bytes. Valid values come back unchanged, so
// usage that only ever had valid values keeps its event IDs. It is for
// values vBilling reads rather than receives (Prometheus labels, gateway
// logs), where dropping the usage would be worse than renaming a value.
func SafeValue(v string) string {
	if CheckValue(v) == nil {
		return v
	}
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(v, "_") {
		if badRune(r) {
			r = '_'
		}
		if b.Len()+utf8.RuneLen(r) > MaxValueLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func badRune(r rune) bool {
	return r == utf8.RuneError || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) ||
		strings.ContainsRune(unsafeValueChars, r)
}
