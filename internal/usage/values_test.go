package usage

import (
	"strings"
	"testing"
)

// Invisible characters are built from their code points so the source
// itself stays plain.
var (
	rlo  = string(rune(0x202E)) // right-to-left override
	zwsp = string(rune(0x200B)) // zero width space
	lsep = string(rune(0x2028)) // line separator
	repl = string(rune(0xFFFD)) // replacement character
)

func TestCheckValueRefusesMarkupAndControlCharacters(t *testing.T) {
	for _, ok := range []string{"", "NVIDIA-H100-80GB-HBM3", "meta-llama/Llama-3.1-405B-Instruct-FP8", "bedrock/anthropic.claude-3-5:0",
		"A100 SXM4 (80 GB)", "gpu.nvidia.com", "caf" + string(rune(0xE9)) + "-model", strings.Repeat("x", MaxValueLen)} {
		if err := CheckValue(ok); err != nil {
			t.Errorf("CheckValue(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{`<img src=x>`, `a"b`, "a'b", "a`b", "a&b", "a>b", "tab\there", "new\nline", "nul\x00",
		"bidi" + rlo + "override", "zero" + zwsp + "width", "line" + lsep + "sep", "bad\xffutf8", "repl" + repl + "acement",
		strings.Repeat("x", MaxValueLen+1)} {
		if err := CheckValue(bad); err == nil {
			t.Errorf("CheckValue(%q) = nil, want an error", bad)
		}
	}
}

func TestSafeValueKeepsValidValuesAndRepairsTheRest(t *testing.T) {
	if got := SafeValue("NVIDIA-H100"); got != "NVIDIA-H100" {
		t.Fatalf("a valid value changed: %q", got)
	}
	for in, want := range map[string]string{
		`<img src=x onerror="alert(1)">`: `_img src=x onerror=_alert(1)__`,
		"tenant\x00\n":                   "tenant__",
		"bad\xffutf8":                    "bad_utf8",
		"bidi" + rlo:                     "bidi_",
	} {
		got := SafeValue(in)
		if got != want || CheckValue(got) != nil {
			t.Errorf("SafeValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := SafeValue(strings.Repeat(string(rune(0xE9)), MaxValueLen) + "<")
	if len(long) > MaxValueLen || CheckValue(long) != nil {
		t.Fatalf("a long value is cut to at most %d bytes on a rune boundary: %d bytes, %v", MaxValueLen, len(long), CheckValue(long))
	}
}
