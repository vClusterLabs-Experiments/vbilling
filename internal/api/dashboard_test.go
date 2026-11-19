package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The dashboard's page is served with a Content-Security-Policy that
// allows its inline script and style by hash and nothing else.
func TestDashboardCSPPinsItsInlineScript(t *testing.T) {
	e := setup(t)
	resp := do(t, http.MethodGet, e.srv.URL+"/", "", nil)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || csp == "" {
		t.Fatalf("GET /: %d, CSP %q", resp.StatusCode, csp)
	}
	for _, block := range []*regexp.Regexp{inlineScript, inlineStyle} {
		m := block.FindSubmatch(page)
		if m == nil {
			t.Fatalf("no %s in the page", block)
		}
		sum := sha256.Sum256(m[1])
		if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
			t.Errorf("the CSP does not pin the served block %s by %s: %s", block, want, csp)
		}
	}
	for _, want := range []string{"default-src 'none'", "script-src-attr 'none'", "frame-ancestors 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "unsafe-hashes") {
		t.Errorf("the CSP must not allow inline scripts or handlers: %s", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers: %v", resp.Header)
	}
}

// The dashboard only ever puts data into the page as text.
func TestDashboardNeverParsesDataAsHTML(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("dashboard", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "srcdoc"} {
		if strings.Contains(string(src), sink) {
			t.Errorf("the dashboard uses %s", sink)
		}
	}
	if m := regexp.MustCompile(`(?i)\son[a-z]+\s*=|\sstyle\s*=`).Find(src); m != nil {
		t.Errorf("inline event handler or style attribute %q (the CSP blocks them)", m)
	}
}
