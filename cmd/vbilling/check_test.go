package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
)

// TestRunAsVBilling is not a test: it turns the test binary into vbilling.
// The tests below start it as a child process to see the real exit code and
// output of "vbilling check-config" and of start-up, each with an
// environment and a metric catalog of its own (loadStartupConfig declares
// metrics process-wide, so it runs once per process).
func TestRunAsVBilling(t *testing.T) {
	args, ok := os.LookupEnv("VBILLING_TEST_ARGS")
	if !ok {
		return
	}
	os.Args = []string{"vbilling"}
	if args != "" {
		os.Args = append(os.Args, strings.Split(args, "\x1f")...)
	}
	main()
	os.Exit(0)
}

type run struct {
	code           int
	stdout, stderr string
}

// vbilling runs the binary with exactly env (nothing of the developer's shell)
// and no way to reach a cluster: start-up with a configuration that passes
// stops at "kubernetes config".
func vbilling(t *testing.T, env map[string]string, args ...string) run {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunAsVBilling$")
	cmd.Env = []string{
		"VBILLING_TEST_ARGS=" + strings.Join(args, "\x1f"),
		"HOME=" + home,
		"KUBECONFIG=" + filepath.Join(home, "no-kubeconfig"),
	}
	if _, ok := env["TMPDIR"]; !ok {
		cmd.Env = append(cmd.Env, "TMPDIR="+t.TempDir())
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := run{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		r.code = exit.ExitCode()
	default:
		t.Fatalf("run vbilling: %v", err)
	}
	return r
}

func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// caPEM is a self-signed CA certificate, as platform.caSecret holds one.
func caPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// A configuration vBilling refuses is refused by check-config with the same
// text, for every setting and every file start-up reads. The validation of
// v0.3.2 found the six invalid Prometheus metric definitions below
// only when the pod restarted, with nothing metering until the change was
// reverted; an invalid enforcement rule only after the wait for the
// Lease.
func TestCheckConfigStopsWithTheStartupErrorOnEachInvalidInput(t *testing.T) {
	const file = "{file}"
	missing := filepath.Join(t.TempDir(), "not-there.yaml")
	cases := []struct {
		name    string
		env     map[string]string // {file} is the path of content
		content string
		want    string // what start-up stops with; {file} is the path
		// afterLease: start-up reaches this check only with the Lease held
		// and its data open, through the same constructor check-config
		// calls (TestStartupStopsOnNothingCheckConfigCannotSee).
		afterLease bool
		// credential: all that is wrong is a credential that is not set or
		// a file from a Secret that is not mounted, which
		// --allow-missing-secrets turns into a warning.
		credential bool
	}{
		// PROMETHEUS_METRICS_FILE: the six cases of the validation.
		{name: "prometheus metric with neither counter nor query", env: map[string]string{"PROMETHEUS_METRICS_FILE": file},
			content: "- {code: vbtest_requests, unit: requests}\n",
			want:    `PROMETHEUS_METRICS_FILE: {file}: metric "vbtest_requests" needs a counter (exact) or a query (estimate)`},
		{name: "prometheus metric with both counter and query", env: map[string]string{"PROMETHEUS_METRICS_FILE": file},
			content: "- {code: vbtest_requests, counter: 'vbtest_requests_total{{selector}}', query: 'sum(vbtest_requests_total)'}\n",
			want:    `PROMETHEUS_METRICS_FILE: {file}: metric "vbtest_requests" has both counter and query; pick one`},
		{name: "prometheus counter that is an expression", env: map[string]string{"PROMETHEUS_METRICS_FILE": file},
			content: "- {code: vbtest_requests, counter: 'rate(vbtest_requests_total[5m])'}\n",
			want:    `PROMETHEUS_METRICS_FILE: {file}: metric "vbtest_requests": counter must be a plain series selector such as vllm:generation_tokens_total{{selector}}, not "rate(vbtest_requests_total[5m])"`},
		{name: "prometheus metric with an invalid code", env: map[string]string{"PROMETHEUS_METRICS_FILE": file},
			content: "- {code: VBTest-Requests, counter: 'vbtest_requests_total{{selector}}'}\n",
			want:    `PROMETHEUS_METRICS_FILE: {file}: custom metric code "VBTest-Requests" must match ^[a-z][a-z0-9_]{0,99}$`},
		{name: "prometheus metric declared twice", env: map[string]string{"PROMETHEUS_METRICS_FILE": file},
			content: "- {code: vbtest_requests, counter: 'a_total{{selector}}'}\n- {code: vbtest_requests, counter: 'b_total{{selector}}'}\n",
			want:    `PROMETHEUS_METRICS_FILE: {file}: metric "vbtest_requests" already exists`},
		{name: "prometheus metric with an unknown field", env: map[string]string{"PROMETHEUS_METRICS_FILE": file},
			content: "- {code: vbtest_requests, counter: 'vbtest_requests_total{{selector}}', window: 5m}\n",
			want:    `PROMETHEUS_METRICS_FILE: {file}: error unmarshaling JSON: while decoding JSON: json: unknown field "window"`},
		{name: "prometheus metrics file that is not there", env: map[string]string{"PROMETHEUS_METRICS_FILE": missing},
			want: "PROMETHEUS_METRICS_FILE: open " + missing + ": no such file or directory"},

		{name: "custom metrics file with an unknown field", env: map[string]string{"CUSTOM_METRICS_FILE": file},
			content: "- {code: inference_output_tokens, unit: tokens, price: 2}\n",
			want:    `CUSTOM_METRICS_FILE {file}: error unmarshaling JSON: while decoding JSON: json: unknown field "price"`},
		{name: "custom metrics file that is not there", env: map[string]string{"CUSTOM_METRICS_FILE": missing},
			want: "CUSTOM_METRICS_FILE: open " + missing + ": no such file or directory"},
		{name: "custom metric that redeclares a built-in one", env: map[string]string{"CUSTOM_METRICS_FILE": file},
			content: "- {code: vcluster_gpu_hours, unit: gpu-hours}\n",
			want:    `CUSTOM_METRICS: metric "vcluster_gpu_hours" already exists`},
		{name: "custom metrics string with too many parts", env: map[string]string{"CUSTOM_METRICS": "tokens:tokens:model:extra"},
			want: `CUSTOM_METRICS: custom metric "tokens:tokens:model:extra": expected code:unit[:key|key]`},

		{name: "records format without usage", env: map[string]string{"RECORDS_MAPPINGS_FILE": file},
			content: "kong: {id: request.id, time: started_at}\n",
			want:    `RECORDS_MAPPINGS_FILE: {file}: format "kong" needs id, time and usage`},
		{name: "records format that declares an invalid metric", env: map[string]string{"RECORDS_MAPPINGS_FILE": file},
			content: "kong: {id: request.id, time: started_at, usage: {Tokens: ai.tokens}, metrics: [{code: Tokens}]}\n",
			want:    `records formats: format kong: custom metric code "Tokens" must match ^[a-z][a-z0-9_]{0,99}$`},
		{name: "records pull of a format that is not configured", env: map[string]string{"RECORDS_PULL_FILE": file},
			content: "- {name: kong-logs, format: kong, url: 'https://logs.example.com/requests?from={{from}}&to={{to}}'}\n",
			want:    `RECORDS_PULL_FILE: {file}: pull 0 (kong-logs): format "kong" is not configured (declare it in records.formats / RECORDS_MAPPINGS_FILE)`},
		{name: "records pull with a lookback past the retention", env: map[string]string{"RECORDS_PULL_FILE": file},
			content: "- {name: gateway-logs, format: vbilling, url: 'https://logs.example.com/requests?from={{from}}', lookback: 200h}\n",
			want:    `RECORDS_PULL_FILE: pull gateway-logs: lookback 200h0m0s must be at most RECORDS_RETENTION minus 2h (166h0m0s)`},
		{name: "records pull pinned to a tenant with two tokens",
			env:     map[string]string{"RECORDS_PULL_FILE": file, "INGEST_TOKENS": "token-one=acme,token-two=acme", "API_TOKEN": "api", "INGEST_TOKEN": "ingest"},
			content: "- {name: gateway-logs, format: vbilling, tenant: acme, url: 'https://logs.example.com/requests?from={{from}}'}\n",
			want: `RECORDS_PULL_FILE: pull gateway-logs: tenant acme has 2 tokens in INGEST_TOKENS: a pull pinned to a tenant stores its rows under the record IDs of the tenant's bound token, ` +
				`and cannot tell which of them the gateway it reads pushes with. Bind one token to acme, the one that gateway pushes with, or do not pin the pull`},

		{name: "tenant clusters file without a kubeconfig", env: map[string]string{"TENANT_CLUSTERS_FILE": file},
			content: "- {name: standalone-1}\n",
			want:    `TENANT_CLUSTERS_FILE: {file}: every tenant cluster needs name and kubeconfig`},
		{name: "tenant clusters file that is not there", env: map[string]string{"TENANT_CLUSTERS_FILE": missing}, credential: true,
			want: "TENANT_CLUSTERS_FILE: open " + missing + ": no such file or directory"},

		{name: "platform CA file that is not a certificate",
			env:     map[string]string{"TENANT_SOURCE": "platform", "PLATFORM_URL": "https://platform.example.com", "PLATFORM_ACCESS_KEY": "key", "PLATFORM_CA_FILE": file},
			content: "not a certificate\n",
			want:    `vCluster Platform: unable to load root certificates: unable to parse bytes as PEM block`},
		{name: "platform URL that is not a URL",
			env:  map[string]string{"TENANT_SOURCE": "platform", "PLATFORM_URL": "platform.example.com", "PLATFORM_ACCESS_KEY": "key"},
			want: `vCluster Platform: PLATFORM_URL "platform.example.com" must be an http(s) URL`},
		{name: "platform without an access key", env: map[string]string{"TENANT_SOURCE": "platform", "PLATFORM_URL": "https://platform.example.com"}, credential: true,
			want: `invalid configuration: TENANT_SOURCE=platform needs PLATFORM_URL and PLATFORM_ACCESS_KEY`},
		{name: "platform without a URL", env: map[string]string{"TENANT_SOURCE": "platform", "PLATFORM_ACCESS_KEY": "key"},
			want: `invalid configuration: TENANT_SOURCE=platform needs PLATFORM_URL and PLATFORM_ACCESS_KEY`},
		{name: "LiteLLM enforcement without its key", env: map[string]string{"ENFORCEMENT_LITELLM": "true", "LITELLM_URL": "http://litellm:4000"}, credential: true,
			want: `invalid configuration: ENFORCEMENT_LITELLM needs LITELLM_URL and LITELLM_API_KEY (a LiteLLM admin key)`},

		{name: "setting with a value that is not allowed", env: map[string]string{"CPU_MEMORY_BASIS": "limits"},
			want: `invalid configuration: CPU_MEMORY_BASIS "limits" must be usage, requests or max`},
		{name: "cross-check of an unknown metric", env: map[string]string{"CROSS_CHECKS": "inference_output_tokens=vllm_output_tokens:2%"},
			want: `CROSS_CHECKS: cross-check "inference_output_tokens=vllm_output_tokens:2%": unknown metric "inference_output_tokens"`},
		{name: "enforcement rule with an unknown state", env: map[string]string{"ENFORCEMENT_RULES": "alert:vbtest-bad=frozen"},
			want: `ENFORCEMENT_RULES: enforcement rule "alert:vbtest-bad=frozen" must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)`},
		{name: "pause keeps an unknown kind billing", env: map[string]string{"PAUSE_KEEP_BILLING": "storage,gpus"},
			want: `PAUSE_KEEP_BILLING: unknown "gpus" (use storage, load_balancers, nodes, control_plane, workloads, or none)`},

		// With the Lease held at start-up; against an empty directory here.
		{name: "adapter that does not exist", env: map[string]string{"ADAPTERS": "noop,chargebee"}, afterLease: true,
			want: `initialize adapters: adapter chargebee: unknown billing adapter "chargebee" (known: [lago metronome noop stripe webhook])`},
		{name: "adapter without its credential", env: map[string]string{"ADAPTERS": "metronome"}, afterLease: true, credential: true,
			want: `initialize adapters: adapter metronome: METRONOME_API_TOKEN is required when the metronome adapter is enabled`},
		{name: "stripe price card with an invalid interval", env: map[string]string{"ADAPTERS": "stripe", "STRIPE_API_KEY": "sk_test_x", "STRIPE_PRICE_CARD": file}, afterLease: true,
			content: "currency: usd\ninterval: fortnight\nplans:\n  dedicated: {vcluster_gpu_hours: {amount: \"2.20\"}}\n",
			want:    `initialize adapters: adapter stripe: STRIPE_PRICE_CARD: {file}: interval "fortnight" must be day, week, month or year`},
		{name: "lago price card with an unknown field", env: map[string]string{"ADAPTERS": "lago", "LAGO_API_KEY": "key", "LAGO_PRICE_CARD": file}, afterLease: true,
			content: "vcluster_gpu_hours: {amount: \"0\", skus: {NVIDIA-H100-80GB-HBM3: \"2.49\"}}\n",
			want:    `initialize adapters: adapter lago: LAGO_PRICE_CARD: {file}: json: unknown field "skus"`},
		{name: "tenant ingest token that is the API token", env: map[string]string{"API_TOKEN": "api", "INGEST_TOKEN": "ingest", "INGEST_TOKENS": "api=acme"}, afterLease: true,
			want: `API token: the INGEST_TOKENS token of tenant acme is also the API or shared ingest token; give each tenant its own token`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := ""
			if tc.content != "" {
				path = write(t, "input.yaml", tc.content)
			}
			env := map[string]string{"ADAPTERS": "noop"}
			for k, v := range tc.env {
				env[k] = strings.ReplaceAll(v, file, path)
			}
			want := strings.ReplaceAll(tc.want, file, path)

			check := vbilling(t, env, "check-config")
			if check.code != 1 || !strings.Contains(check.stderr, "error    "+want+"\n") {
				t.Errorf("check-config: exit %d, want 1 with\n  %s\nstderr:\n%s", check.code, want, check.stderr)
			}
			if !strings.Contains(check.stderr, "vBilling would stop at start-up") || strings.Contains(check.stderr, "The configuration is valid") {
				t.Errorf("check-config must say the configuration is not valid:\n%s", check.stderr)
			}
			// One stream for the whole report: a pod's log keeps no order between two.
			settings, failure := strings.Index(check.stderr, "ok       settings: "), strings.Index(check.stderr, "error    ")
			if check.stdout != "" || (settings >= 0 && settings > failure) {
				t.Errorf("a failed check reports on standard error alone, in order:\nstdout:\n%s\nstderr:\n%s", check.stdout, check.stderr)
			}
			// The flag forgives a credential that is not set, and nothing else.
			lenient := vbilling(t, env, "check-config", "--allow-missing-secrets")
			switch {
			case tc.credential && (lenient.code != 0 || !strings.Contains(lenient.stdout, "warning  ")):
				t.Errorf("check-config --allow-missing-secrets: exit %d, want 0 with a warning\n%s%s", lenient.code, lenient.stdout, lenient.stderr)
			case !tc.credential && (lenient.code != 1 || !strings.Contains(lenient.stderr, "error    "+want+"\n")):
				t.Errorf("check-config --allow-missing-secrets must stay strict here: exit %d\n%s", lenient.code, lenient.stderr)
			}

			start := vbilling(t, env)
			if tc.afterLease {
				// Without a cluster, start-up cannot get as far as this check.
				if start.code != 1 || !strings.Contains(start.stderr, "kubernetes config: ") {
					t.Errorf("start-up: exit %d, want it to pass the configuration and stop at the Kubernetes config:\n%s", start.code, start.stderr)
				}
				return
			}
			if start.code != 1 || !strings.Contains(start.stderr, ": "+want+"\n") {
				t.Errorf("start-up: exit %d, want 1 with the same text\n  %s\nstderr:\n%s", start.code, want, start.stderr)
			}
			if strings.Contains(start.stderr, "kubernetes config") {
				t.Errorf("start-up must refuse the configuration before it looks for a cluster (and its Lease):\n%s", start.stderr)
			}
		})
	}
}

// fullConfiguration is an environment that uses every file and adapter, with
// every URL pointing at base.
func fullConfiguration(t *testing.T, base string) map[string]string {
	t.Helper()
	return map[string]string{
		"ADAPTERS":            "stripe,metronome,lago,webhook",
		"REGION":              "us-east-1",
		"STRIPE_API_KEY":      "sk_test_x",
		"STRIPE_API_BASE":     base,
		"METRONOME_API_TOKEN": "token",
		"METRONOME_API_BASE":  base,
		"LAGO_API_KEY":        "key",
		"LAGO_API_URL":        base,
		"WEBHOOK_URL":         base + "/usage",
		"PROMETHEUS_URL":      base,
		"LITELLM_URL":         base,
		"LITELLM_API_KEY":     "sk-litellm",
		"TENANT_SOURCE":       "platform",
		"PLATFORM_URL":        base,
		"PLATFORM_ACCESS_KEY": "access-key",
		"PLATFORM_CA_FILE":    write(t, "ca.crt", caPEM(t)),
		"API_TOKEN":           "api-token",
		"INGEST_TOKEN":        "ingest-token",
		"INGEST_TOKENS":       "token-acme=acme,token-globex=globex",
		"ENFORCEMENT_MODE":    "enforce",
		"ENFORCEMENT_RULES":   "alerts.low_remaining_commit_balance_reached=paused,alert:Wallet below $50=warning",
		"PAUSE_KEEP_BILLING":  "storage",
		"PRICE_CHECK":         "hold",
		"CROSS_CHECKS":        "inference_output_tokens=vllm_output_tokens:2%",
		"CUSTOM_METRICS":      "slurm_gpu_hours:gpu-hours:partition",
		"ZONE":                "us-east-1a", // removed in v0.3.1: a warning, never an error
		"CUSTOM_METRICS_FILE": write(t, "custom-metrics.yaml",
			"- {code: inference_output_tokens, unit: tokens, groupKeys: [sku]}\n- {code: vllm_output_tokens, unit: tokens, billable: false}\n"),
		"RECORDS_MAPPINGS_FILE": write(t, "records-formats.yaml",
			"kong: {id: request.id, time: started_at, tenant: consumer.username, usage: {inference_output_tokens: ai.completion_tokens}}\n"),
		"RECORDS_PULL_FILE": write(t, "records-pull.yaml",
			"- {name: kong-logs, format: kong, tenant: acme, url: '"+base+"/requests?from={{from}}&to={{to}}', headers: {Authorization: 'Bearer ${RECORDS_PULL_KONG_TOKEN}'}}\n"),
		"PROMETHEUS_METRICS_FILE": write(t, "metrics.yaml",
			"- {code: vllm_generation_tokens, unit: tokens, skuLabel: model_name, counter: 'vllm:generation_tokens_total{{selector}}'}\n"),
		"STRIPE_PRICE_CARD": write(t, "stripe-price-card.yaml",
			"currency: usd\nplans:\n  dedicated: {vcluster_gpu_hours: {sku: {NVIDIA-H100-80GB-HBM3: \"2.49\"}}}\n"),
		"LAGO_PRICE_CARD":      write(t, "price-card.yaml", "vcluster_gpu_hours: {amount: \"0\", sku: {NVIDIA-H100-80GB-HBM3: \"2.49\"}}\n"),
		"TENANT_CLUSTERS_FILE": write(t, "clusters.yaml", "- {name: standalone-1, kubeconfig: standalone-1.kubeconfig, tenant: acme}\n"),
	}
}

// A valid configuration: check-config says what it checked and exits 0. It
// contacts none of the URLs it is given, creates neither DATA_DIR nor
// ARCHIVE_DIR, and leaves nothing behind in its temporary directory.
func TestCheckConfigListsWhatItCheckedAndTouchesNothing(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()

	volumes, tmp := t.TempDir(), t.TempDir()
	env := fullConfiguration(t, srv.URL)
	env["DATA_DIR"] = filepath.Join(volumes, "data")
	env["ARCHIVE_DIR"] = filepath.Join(volumes, "archive", "vbilling-0")
	env["TMPDIR"] = tmp

	r := vbilling(t, env, "check-config")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"vBilling " + version + ": checking the configuration as start-up reads it",
		"ok       settings: adapters stripe,metronome,lago,webhook, region us-east-1, tenant source platform, enforcement enforce, price check hold",
		"ok       CUSTOM_METRICS: 1 metric(s)",
		"ok       CUSTOM_METRICS_FILE " + env["CUSTOM_METRICS_FILE"] + ": 2 metric(s)",
		"ok       RECORDS_MAPPINGS_FILE " + env["RECORDS_MAPPINGS_FILE"] + ": 2 records format(s) with the built-in ones",
		"ok       PROMETHEUS_METRICS_FILE " + env["PROMETHEUS_METRICS_FILE"] + ": 1 metric(s)",
		"ok       vCluster Platform: " + srv.URL + ", CA " + env["PLATFORM_CA_FILE"],
		"ok       PAUSE_KEEP_BILLING: storage",
		"ok       TENANT_CLUSTERS_FILE " + env["TENANT_CLUSTERS_FILE"] + ": 1 cluster(s)",
		"ok       CROSS_CHECKS: 1 check(s)",
		"ok       ENFORCEMENT_RULES: 2 rule(s)",
		"ok       RECORDS_PULL_FILE " + env["RECORDS_PULL_FILE"] + ": 1 pull(s)",
		"ok       adapters stripe,metronome,lago,webhook: settings, STRIPE_PRICE_CARD " + env["STRIPE_PRICE_CARD"] + ", LAGO_PRICE_CARD " + env["LAGO_PRICE_CARD"],
		"ok       ingest tokens: 2 bound to a tenant, none of them the API token or the shared ingest token",
		"warning  setting ZONE is no longer read: the zone comes from each node's topology.kubernetes.io/zone label",
		"The configuration is valid: 13 check(s) passed, 1 warning(s).",
	} {
		if !strings.Contains(r.stdout, want+"\n") {
			t.Errorf("stdout lacks %q", want)
		}
	}
	// What an adapter logs when it is set up is in the report, not beside it.
	if !strings.Contains(r.stdout, "\nnote     [metronome] ") || r.stderr != "" {
		t.Errorf("the report is one stream, the adapters' log lines included; stderr:\n%s", r.stderr)
	}
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("check-config made %d request(s): it must contact nothing", n)
	}
	if entries, _ := os.ReadDir(volumes); len(entries) != 0 {
		t.Errorf("check-config created %v under the volumes: it must open neither DATA_DIR nor ARCHIVE_DIR", entries)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("check-config left %v in its temporary directory", entries)
	}

	// Start-up accepts the same configuration: it logs the same warning and
	// goes on to Kubernetes (there is none here), before any Lease or data.
	start := vbilling(t, env)
	if start.code != 1 || !strings.Contains(start.stderr, "kubernetes config: ") ||
		!strings.Contains(start.stderr, "WARNING: setting ZONE is no longer read: the zone comes from each node's topology.kubernetes.io/zone label\n") {
		t.Errorf("start-up: exit %d, want the ZONE warning and then the stop at the Kubernetes config:\n%s", start.code, start.stderr)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("start-up made %d request(s) before it had its Lease", n)
	}
	if entries, _ := os.ReadDir(volumes); len(entries) != 0 {
		t.Errorf("start-up created %v before it had its Lease", entries)
	}
}

// check-config writes nothing, so it also runs where nothing can be written:
// `kubectl exec` into the vBilling pod, whose root filesystem is read-only,
// to check a ConfigMap edited by hand before the pod is restarted. The
// adapters keep state and the API tokens are generated in DATA_DIR at
// start-up; here neither is.
func TestCheckConfigRunsOnAReadOnlyFilesystem(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"TMPDIR":         readOnly,
		"DATA_DIR":       filepath.Join(readOnly, "data"),
		"ADAPTERS":       "stripe,lago",
		"STRIPE_API_KEY": "sk_test_x",
		"LAGO_API_KEY":   "key",
		"INGEST_TOKENS":  "token-acme=acme",
		// No API_TOKEN and no INGEST_TOKEN: vBilling would generate both.
	}
	r := vbilling(t, env, "check-config")
	if r.code != 0 || !strings.Contains(r.stdout, "ok       adapters stripe,lago: settings\n") ||
		!strings.Contains(r.stdout, "ok       ingest tokens: 1 bound to a tenant") {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if entries, _ := os.ReadDir(readOnly); len(entries) != 0 {
		t.Fatalf("check-config wrote %v", entries)
	}
}

// The chart's Job runs before the release's Secret exists. With
// --allow-missing-secrets, a credential that is not set and a file from a
// Secret that is not mounted are warnings, and everything that does not
// need them is still checked.
func TestCheckConfigAllowMissingSecrets(t *testing.T) {
	notMounted := filepath.Join(t.TempDir(), "not-mounted")
	env := fullConfiguration(t, "https://billing.example.com")
	for _, secret := range []string{"STRIPE_API_KEY", "METRONOME_API_TOKEN", "LAGO_API_KEY", "LITELLM_API_KEY", "PLATFORM_ACCESS_KEY",
		"API_TOKEN", "INGEST_TOKEN", "INGEST_TOKENS"} {
		delete(env, secret)
	}
	env["TENANT_CLUSTERS_FILE"] = filepath.Join(notMounted, "clusters.yaml")
	env["PLATFORM_CA_FILE"] = filepath.Join(notMounted, "ca.crt")

	strict := vbilling(t, env, "check-config")
	if strict.code != 1 || !strings.Contains(strict.stderr, "error    invalid configuration: TENANT_SOURCE=platform needs PLATFORM_URL and PLATFORM_ACCESS_KEY\n") {
		t.Fatalf("without the flag a missing credential is an error: exit %d\n%s", strict.code, strict.stderr)
	}

	r := vbilling(t, env, "check-config", "--allow-missing-secrets")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"warning  LAGO_API_KEY is not set here, and the lago adapter needs it: it is not checked, and vBilling does not start without it",
		"warning  STRIPE_API_KEY is not set here, and the stripe adapter needs it: it is not checked, and vBilling does not start without it",
		"warning  METRONOME_API_TOKEN is not set here, and the metronome adapter needs it: it is not checked, and vBilling does not start without it",
		"warning  PLATFORM_ACCESS_KEY is not set here, and TENANT_SOURCE=platform needs it: it is not checked, and vBilling does not start without it",
		"warning  LITELLM_API_KEY is not set here, and LITELLM_URL needs it: it is not checked, and vBilling does not start without it",
		"warning  PLATFORM_CA_FILE " + env["PLATFORM_CA_FILE"] + " is not there (it is mounted from a Secret): not checked here",
		"warning  TENANT_CLUSTERS_FILE " + env["TENANT_CLUSTERS_FILE"] + " is not there (it is mounted from a Secret): not checked here",
		// What does not depend on a Secret is checked all the same.
		"ok       PROMETHEUS_METRICS_FILE " + env["PROMETHEUS_METRICS_FILE"] + ": 1 metric(s)",
		"ok       adapters stripe,metronome,lago,webhook: settings, STRIPE_PRICE_CARD " + env["STRIPE_PRICE_CARD"] + ", LAGO_PRICE_CARD " + env["LAGO_PRICE_CARD"],
		"The configuration is valid: 12 check(s) passed, 8 warning(s).",
	} {
		if !strings.Contains(r.stdout, want+"\n") {
			t.Errorf("stdout lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("stdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
	}

	// Still strict: a price card is not a credential.
	env["STRIPE_PRICE_CARD"] = write(t, "stripe-price-card.yaml", "currency: usd\nplans: {}\n")
	bad := vbilling(t, env, "check-config", "--allow-missing-secrets")
	want := "error    initialize adapters: adapter stripe: STRIPE_PRICE_CARD: " + env["STRIPE_PRICE_CARD"] + ": no plans\n"
	if bad.code != 1 || !strings.Contains(bad.stderr, want) {
		t.Fatalf("an invalid price card must fail with or without credentials: exit %d\n%s", bad.code, bad.stderr)
	}
}

// requiredCredentials must know every credential an adapter refuses to start
// without: an adapter added without its entry would fail every install here,
// in the chart's Job, before its Secret exists.
func TestEveryAdapterPassesTheCheckWithoutItsCredentials(t *testing.T) {
	for _, name := range destinations.Registered() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := vbilling(t, map[string]string{"ADAPTERS": name, "WEBHOOK_URL": "https://sink.example.com/usage"}, "check-config", "--allow-missing-secrets")
			if r.code != 0 {
				t.Fatalf("adapter %s without credentials: exit %d\n%s%s\nadd the credential it requires to requiredCredentials (check.go)", name, r.code, r.stdout, r.stderr)
			}
		})
	}
}

func TestCheckConfigUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := checkConfigMain([]string{"-h"}, &out, &errOut); code != 0 || !strings.Contains(errOut.String(), "Usage: vbilling check-config [--allow-missing-secrets]") ||
		!strings.Contains(errOut.String(), "-allow-missing-secrets") {
		t.Fatalf("-h: exit %d\n%s", code, errOut.String())
	}
	for _, args := range [][]string{{"--no-such-flag"}, {"extra"}} {
		errOut.Reset()
		if code := checkConfigMain(args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, errOut.String())
		}
	}
	if out.Len() != 0 {
		t.Fatalf("nothing is checked on a usage error, got:\n%s", out.String())
	}
}

// Start-up must not stop on a setting check-config does not check: what only
// main checks, the chart's Job cannot refuse before the pod is replaced.
//
// main.go may parse a setting again where it uses it (the parsers are pure
// and see what loadStartupConfig accepted), but every log.Fatalf of main.go
// that names a setting, and the three that wrap a constructor both sides
// call, must have its text in check.go.
func TestStartupStopsOnNothingCheckConfigCannotSee(t *testing.T) {
	fatal := fatalFormats(t, "main.go")
	checked := map[string]bool{}
	for _, s := range stringLiterals(t, "check.go") {
		checked[s] = true
	}
	inCheck := func(format string) bool {
		return checked[format] || checked[strings.ReplaceAll(format, "%v", "%w")]
	}
	if len(fatal) < 10 {
		t.Fatalf("found only %d log.Fatalf calls in main.go: this test no longer reads it", len(fatal))
	}
	setting := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+\b`)
	for _, f := range fatal {
		if setting.MatchString(f) && !inCheck(f) {
			t.Errorf("main.go stops with %q, which check.go does not: move the check into loadStartupConfig, "+
				"so \"vbilling check-config\" and the chart's pre-upgrade Job make it too", f)
		}
	}
	for _, both := range []string{"initialize adapters: %v", "API token: %v", "vCluster Platform: %v"} {
		if !slices.Contains(fatal, both) || !inCheck(both) {
			t.Errorf("start-up and check-config must both stop with %q: one of them changed its text or lost the check", both)
		}
	}
}

// fatalFormats returns the format strings of the log.Fatalf calls in a file
// of this package, sorted.
func fatalFormats(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Fatalf" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "log" {
			return true
		}
		if s, ok := literal(call.Args[0]); ok {
			out = append(out, s)
		}
		return true
	})
	sort.Strings(out)
	return out
}

// stringLiterals returns every string literal of a file of this package,
// with concatenations ("a" + "b") joined.
func stringLiterals(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok {
			if s, ok := literal(e); ok {
				out = append(out, s)
				return false
			}
		}
		return true
	})
	return out
}

func literal(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok1 := literal(v.X)
		r, ok2 := literal(v.Y)
		return l + r, ok1 && ok2
	}
	return "", false
}
