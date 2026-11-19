package usage

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxValueLen caps a SKU, a dimension value, a region, a project or a
// resource ID, in bytes. GPU products (NVIDIA-H100-80GB-HBM3), model names
// with a provider prefix and node names (at most 253 characters) all fit.
const MaxValueLen = 256

// MaxDimensions is the most dimensions an ingested event or record may
// carry. vBilling's own events carry at most about fifteen; every
// dimension is copied into each usage row of a record and into every
// billing platform's copy of the event.
const MaxDimensions = 32

// unsafeValueChars have a meaning in HTML, attributes or scripts. SKUs and
// dimension values reach dashboards, exports and billing platforms' own
// pages, and some of them are chosen by tenants, so they never contain
// these characters. They do not make a value safe in a spreadsheet: a
// value may still start with = + - or @, so CSV exports escape such cells
// themselves (CSVCell).
const unsafeValueChars = "<>\"'`&"

// CheckValue reports why v cannot be a SKU, a dimension value, a region, a
// project or a resource ID: it is longer than MaxValueLen bytes, is not
// UTF-8, or contains a control or formatting character or one of
// < > " ' ` &.
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

// reservedDimensions are the properties billing platforms receive for an
// event itself, next to its dimensions: Metronome takes both as one
// property map, so a dimension with one of these names would replace the
// quantity, the SKU or the window it is billed with.
var reservedDimensions = map[string]bool{
	"value": true, "unit": true, "sku": true, "region": true, "project": true, "resource_id": true, "source": true,
	"window_start": true, "window_end": true, "schema_version": true, "accuracy": true, "estimate": true,
	DimMeteringSource: true, // assigned by the collector, never by an emitter
}

// ReservedDimension reports whether k names a property of the event itself
// (value, unit, sku, region, project, resource_id, source, window_start,
// window_end, schema_version, accuracy, estimate, metering_source), which no emitter dimension may
// use.
func ReservedDimension(k string) bool { return reservedDimensions[k] }

// CSVCell returns v ready for a CSV export cell. A cell that starts with
// = + - or @ (or a tab or carriage return) is a formula to a spreadsheet,
// and values such as tenants, IDs and SKUs come from tenants and gateways:
// such a cell gets a leading ' so it is shown as text.
func CSVCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
