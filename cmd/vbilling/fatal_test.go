package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSelf starts this test binary again to run one test in a process of its
// own, with extra environment, and returns what it printed and its exit
// code.
func runSelf(t *testing.T, test string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("run %s: %v", test, err)
	return "", 0
}

// A start-up error after the Lease is taken ends the process with
// log.Fatal, which skips the deferred release: the Lease stayed held by a
// process that was gone, and every restart waited about 35 seconds for it
// before failing again. During start-up a log.Fatal now gives the Lease up
// first; once start-up is over it does not, so that nothing takes over
// while the process could still be writing.
func TestFatalDuringStartupGivesTheLeaseUp(t *testing.T) {
	if marker := os.Getenv("VBILLING_TEST_FATAL_MARKER"); marker != "" {
		// The process under test.
		startupOver := releaseOnFatal(func() { _ = os.WriteFile(marker, []byte("released"), 0o600) })
		log.Printf("an ordinary line gives nothing up")
		if _, err := os.Stat(marker); err == nil {
			os.Exit(3)
		}
		if os.Getenv("VBILLING_TEST_STARTUP_OVER") != "" {
			startupOver()
		}
		log.Fatalf("open ledger: no space left on device")
	}
	for _, tc := range []struct {
		name     string
		env      []string
		released bool
	}{
		{"during start-up", nil, true},
		{"once start-up is over", []string{"VBILLING_TEST_STARTUP_OVER=1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "released")
			out, code := runSelf(t, "TestFatalDuringStartupGivesTheLeaseUp", append(tc.env, "VBILLING_TEST_FATAL_MARKER="+marker)...)
			if code != 1 || !strings.Contains(out, "open ledger: no space left on device") {
				t.Fatalf("exit code %d, output:\n%s\nwant the fatal line and exit code 1", code, out)
			}
			_, err := os.Stat(marker)
			if released := err == nil; released != tc.released {
				t.Fatalf("Lease given up: %t, want %t", released, tc.released)
			}
		})
	}
}

// An enforcement rule that cannot be used stops vBilling before it looks
// for a cluster, and so before it waits for and takes the Lease: with no
// cluster to reach at all, the process still exits on the rule, with the
// line start-up always printed for it.
func TestInvalidEnforcementRuleStopsVBillingBeforeTheLease(t *testing.T) {
	if os.Getenv("VBILLING_TEST_MAIN") != "" {
		main() // the process under test
		return
	}
	out, code := runSelf(t, "TestInvalidEnforcementRuleStopsVBillingBeforeTheLease", "VBILLING_TEST_MAIN=1",
		"ENFORCEMENT_RULES=alert:vbtest-bad=frozen", "ADAPTERS=noop", "DATA_DIR="+t.TempDir(),
		"KUBECONFIG="+filepath.Join(t.TempDir(), "no-such-kubeconfig"), "KUBERNETES_SERVICE_HOST=", "KUBERNETES_SERVICE_PORT=")
	want := `ENFORCEMENT_RULES: enforcement rule "alert:vbtest-bad=frozen" must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)`
	if code != 1 || !strings.Contains(out, want) {
		t.Fatalf("exit code %d, output:\n%s\nwant exit code 1 and: %s", code, out, want)
	}
	for _, later := range []string{"kubernetes config", "Lease", "Using kubeconfig", "Adapters:"} {
		if strings.Contains(out, later) {
			t.Fatalf("vBilling went on past the invalid rule (%q in its output):\n%s", later, out)
		}
	}
}
