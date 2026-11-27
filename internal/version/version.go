// Package version is the one place vBilling's version is written.
//
// The start-up log line, GET /api/v1/status and the User-Agent of every
// outgoing client (the billing adapters, Prometheus, vCluster Platform) read
// it from here, so they cannot disagree. A release bumps Version together
// with the chart's version, appVersion and image tag; version_test.go fails
// when they differ, and when a "vbilling/<number>" literal is written
// anywhere else.
package version

// Version is the vBilling release this binary is built from. It is a
// variable so a build can set it:
//
//	go build -ldflags "-X github.com/vclusterlabs-experiments/vbilling/internal/version.Version=1.2.3"
var Version = "0.3.3"

// UserAgent is what vBilling sends as User-Agent on every request it makes:
// "vbilling/" and the version.
func UserAgent() string { return "vbilling/" + Version }
