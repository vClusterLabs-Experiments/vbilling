package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// dashboardCSP is the dashboard's Content-Security-Policy. Its inline
// script and style are allowed by their SHA-256, computed from the embedded
// page at startup, so no other script runs: not one injected into the page,
// not an inline event handler (onerror=...), not one loaded from elsewhere.
// The page talks only to this server, cannot be framed, and Chromium
// enforces that no string ever reaches an HTML sink (innerHTML and the
// like) through Trusted Types.
var dashboardCSP = buildDashboardCSP()

var (
	inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	inlineStyle  = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
)

func buildDashboardCSP() string {
	page, err := fs.ReadFile(dashboardFS, "dashboard/index.html")
	if err != nil {
		panic("dashboard: " + err.Error())
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + hashSources(inlineScript, page),
		"script-src-attr 'none'",
		"style-src " + hashSources(inlineStyle, page),
		"img-src data:",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
		"require-trusted-types-for 'script'",
	}, "; ")
}

// hashSources lists the CSP hash source of every inline block re matches.
// The browser hashes a block's text exactly as it is in the page.
func hashSources(re *regexp.Regexp, page []byte) string {
	var out []string
	for _, m := range re.FindAllSubmatch(page, -1) {
		sum := sha256.Sum256(m[1])
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	if len(out) == 0 {
		panic("dashboard: no inline block matches " + re.String())
	}
	return strings.Join(out, " ")
}

// dashboardHeaders serves the dashboard's static page with its security
// headers. The page holds no data: everything it shows comes from API
// calls that need the API token.
func dashboardHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", dashboardCSP)
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
