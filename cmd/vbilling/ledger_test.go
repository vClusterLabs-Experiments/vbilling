package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// archivedLedger writes six one-minute windows, one segment each, for tenants
// a and b, and compacts the first three into an archive: windows 10:00-10:02
// are archived, 10:03-10:05 live. It returns DATA_DIR and the archive.
func archivedLedger(t *testing.T) (data, arch string) {
	t.Helper()
	data, arch = t.TempDir(), filepath.Join(t.TempDir(), "archive")
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	now := start
	sp, err := spool.Open(filepath.Join(data, "ledger"), spool.Options{
		SegmentMaxRecords: 3, Retention: time.Hour, ArchiveDir: arch, NoSync: true,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	sp.Cursor("stripe")
	for w := 0; w < 6; w++ {
		ws := start.Add(time.Duration(w) * time.Minute)
		now = ws.Add(time.Minute)
		var evs []usage.Event
		for _, tenant := range []string{"a", "b"} {
			e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: 8.0 / 60, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
			e.Finalize()
			evs = append(evs, e)
		}
		if err := sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Hour)
	sp.Ack("stripe", 9)
	if n, err := sp.Compact([]string{"stripe"}); n != 3 || err != nil {
		t.Fatalf("compact: %d, %v", n, err)
	}
	return data, arch
}

func runLedger(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = ledgerMain(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestLedgerVerify(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	data, arch := archivedLedger(t)

	check := func(code int, out string, wantOK bool, wantRecords uint64) spool.ChainReport {
		t.Helper()
		var rep spool.ChainReport
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("report is not JSON: %v\n%s", err, out)
		}
		if rep.OK != wantOK || (code == 0) != wantOK || rep.Records != wantRecords {
			t.Fatalf("exit %d, report %+v", code, rep)
		}
		return rep
	}
	code, out, _ := runLedger("verify", "--dir", data, "--archive", arch)
	if rep := check(code, out, true, 18); rep.FirstSeq != 1 || rep.Anchor != "" || rep.ArchivedSegments != 3 {
		t.Fatalf("report: %+v", rep)
	}
	// Inside the pod the flags default to DATA_DIR and ARCHIVE_DIR.
	t.Setenv("DATA_DIR", data)
	t.Setenv("ARCHIVE_DIR", arch)
	code, out, _ = runLedger("verify")
	check(code, out, true, 18)
	code, out, _ = runLedger("verify", "--archive", "")
	check(code, out, true, 9)

	// A changed byte in an archived file fails verification with exit 1.
	gz := filepath.Join(arch, "seg-00000000000000000004.log.gz")
	b, err := os.ReadFile(gz)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 1
	os.WriteFile(gz, b, 0o640)
	code, out, _ = runLedger("verify")
	if rep := check(code, out, false, 15); !strings.Contains(strings.Join(rep.Errors, "\n"), "sha256 does not match") {
		t.Fatalf("errors: %v", rep.Errors)
	}
}

func TestLedgerExport(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	data, arch := archivedLedger(t)

	// Tenant b's events from 10:01 to 10:05, across the archive and the ledger.
	code, out, errOut := runLedger("export", "--dir", data, "--archive", arch,
		"--from", "2026-10-05T10:01:00Z", "--to", "2026-10-05T10:05:00Z", "--tenant", "b")
	if code != 0 || !strings.Contains(errOut, "exported 4 event(s)") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var e usage.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if want := time.Date(2026, 10, 5, 10, 1+i, 0, 0, time.UTC); e.Tenant != "b" || !e.WindowStart.Equal(want) || e.ID == "" {
			t.Fatalf("line %d: %+v, want tenant b at %s", i, e, want)
		}
	}
	// Whole days and metric filters; no events of another metric.
	code, out, _ = runLedger("export", "--dir", data, "--archive", arch, "--from", "2026-10-05", "--to", "2026-10-06")
	if code != 0 || strings.Count(out, "\n") != 12 {
		t.Fatalf("whole day: exit %d, %d lines", code, strings.Count(out, "\n"))
	}
	code, out, _ = runLedger("export", "--dir", data, "--archive", arch, "--from", "2026-10-05", "--to", "2026-10-06", "--metric", usage.MetricCPUCoreHours)
	if code != 0 || out != "" {
		t.Fatalf("metric filter: exit %d, %q", code, out)
	}
	// Without the archive, the oldest retained record is past --from: say so.
	code, _, errOut = runLedger("export", "--dir", data, "--from", "2026-10-05", "--to", "2026-10-06")
	if code != 0 || !strings.Contains(errOut, "warning: the oldest retained record") {
		t.Fatalf("ledger alone: exit %d: %s", code, errOut)
	}
	// A missing archived segment fails the export instead of shortening it.
	os.Remove(filepath.Join(arch, "seg-00000000000000000004.log.gz"))
	if code, _, errOut = runLedger("export", "--dir", data, "--archive", arch, "--from", "2026-10-05", "--to", "2026-10-06"); code != 1 || !strings.Contains(errOut, "archived file is missing") {
		t.Fatalf("missing segment: exit %d: %s", code, errOut)
	}
}

func TestLedgerUsageErrors(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	for name, args := range map[string][]string{
		"no command":         nil,
		"unknown command":    {"rewrite"},
		"verify nothing":     {"verify"},
		"unknown flag":       {"verify", "--dirs", "x"},
		"stray argument":     {"verify", "--dir", "x", "y"},
		"export without to":  {"export", "--dir", "x", "--from", "2026-10-05"},
		"export bad time":    {"export", "--dir", "x", "--from", "yesterday", "--to", "2026-10-06"},
		"export empty range": {"export", "--dir", "x", "--from", "2026-10-06", "--to", "2026-10-05"},
	} {
		if code, _, _ := runLedger(args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if code, out, _ := runLedger("help"); code != 0 || !strings.Contains(out, "verify") {
		t.Errorf("help: exit %d", code)
	}
	// A directory that holds no ledger is an error, not an empty success.
	if code, out, _ := runLedger("verify", "--dir", t.TempDir()); code != 1 || !strings.Contains(out, "no ledger in") {
		t.Errorf("empty dir: exit %d: %s", code, out)
	}
}
