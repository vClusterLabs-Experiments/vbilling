package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

const ledgerUsage = `Usage: vbilling ledger <command> [flags]

Offline access to the usage ledger, its long-term archive and its mirror.
verify and export only read: they are safe to run next to a live vBilling
(kubectl exec) or on a copy of the files. restore writes a data directory and
must never run on one a vBilling is using.

Commands:
  verify   check archived and live records as one hash chain (JSON report)
  export   write the usage events of a time range as NDJSON
  restore  bring a lost data directory back from its mirror

Run "vbilling ledger <command> -h" for the flags of a command.
`

// runLedgerCmd runs "vbilling ledger ..." and returns the exit code.
func runLedgerCmd(args []string) int {
	return ledgerMain(args, os.Stdout, os.Stderr)
}

func ledgerMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ledgerUsage)
		return 2
	}
	switch args[0] {
	case "verify":
		return ledgerVerify(args[1:], stdout, stderr)
	case "export":
		return ledgerExport(args[1:], stdout, stderr)
	case "restore":
		return ledgerRestore(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, ledgerUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown ledger command %q\n\n%s", args[0], ledgerUsage)
		return 2
	}
}

// ledgerFlags declares --dir and --archive. Inside the vBilling pod they
// default to the ledger and archive the controller itself uses.
func ledgerFlags(name, usage string, stderr io.Writer) (fs *flag.FlagSet, dir, archive *string) {
	fs = flag.NewFlagSet("vbilling ledger "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	defDir := ""
	if d := os.Getenv("DATA_DIR"); d != "" {
		defDir = filepath.Join(d, "ledger")
	}
	dir = fs.String("dir", defDir, "ledger directory, DATA_DIR/ledger (DATA_DIR also works); empty to skip the live ledger")
	archive = fs.String("archive", os.Getenv("ARCHIVE_DIR"), "archive directory (ARCHIVE_DIR); empty to skip the archive")
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	return fs, dir, archive
}

// parseFlags parses args and reports the exit code to return, if any.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return 2, true
	}
	return 0, false
}

func ledgerVerify(args []string, stdout, stderr io.Writer) int {
	fs, dir, archive := ledgerFlags("verify", `Usage: vbilling ledger verify [--dir DIR] [--archive DIR]

Verifies every archived segment (the gzip file's sha256 from archive.jsonl,
each record's hash, the chain links across segments), then the live ledger,
which must continue exactly where the archive ends. Prints a JSON report and
exits 1 if anything fails.

--dir also takes a mirror directory (ARCHIVE_DIR/mirror): the mirrored ledger
is verified the same way, and "mirror" in the report says whether the
mirror's recovery point, what "vbilling ledger restore" would restore, is
complete: the records of each ledger up to the recorded position and the
sha256 of every state file.

`, stderr)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *dir == "" && *archive == "" {
		fmt.Fprintln(stderr, "set --dir, --archive or both")
		return 2
	}
	var rep struct {
		spool.ChainReport
		Mirror *spool.MirrorReport `json:"mirror,omitempty"`
	}
	rep.ChainReport = spool.VerifyChain(*dir, *archive)
	ok := rep.OK
	if *dir != "" && spool.IsMirror(*dir) {
		m := spool.VerifyMirror(*dir)
		rep.Mirror, ok = &m, ok && m.OK
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

func ledgerRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vbilling ledger restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", spool.MirrorDir(os.Getenv("ARCHIVE_DIR")), "mirror directory, ARCHIVE_DIR/mirror, or a copy that was set aside next to it")
	to := fs.String("to", os.Getenv("DATA_DIR"), "data directory to restore (DATA_DIR)")
	force := fs.Bool("force", false, "replace the ledger the data directory already holds, and everything else in it")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: vbilling ledger restore --from MIRROR_DIR --to DATA_DIR [--force]

Restores a data directory from its mirror after the data volume was lost. The
mirror (ARCHIVE_DIR/mirror) is a copy of the ledgers and of every other file
of DATA_DIR, a few seconds behind. restore verifies it, copies its recovery
point, the state of DATA_DIR when the mirror last completed a pass, verifies
the restored ledgers and prints what it restored and what vBilling does next.

vBilling restores by itself when it starts on a data directory without a
ledger and finds its mirror. Use this command to restore somewhere else, from
a mirror copy that was set aside, or over a ledger that is damaged.

It refuses a data directory that already holds a ledger. --force replaces
that ledger and deletes everything else in the directory: stop vBilling
first, and keep a copy if the ledger there may be needed.

`)
		fs.PrintDefaults()
	}
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *from == "" || *to == "" {
		fmt.Fprintln(stderr, "set --from (the mirror directory) and --to (the data directory)")
		return 2
	}
	rep, err := spool.RestoreMirror(*from, *to, spool.RestoreOptions{Force: *force})
	if err != nil {
		fmt.Fprintf(stderr, "restore failed: %v\n", err)
		if errors.Is(err, spool.ErrNoRecoveryPoint) {
			fmt.Fprintln(stderr, "the mirror directory is ARCHIVE_DIR/mirror; one that was set aside is ARCHIVE_DIR/mirror-set-aside-<time>")
		}
		return 1
	}
	printRestore(stdout, rep, time.Now())
	return 0
}

// printRestore writes what a restore brought back and what vBilling does
// with it at its next start.
func printRestore(w io.Writer, rep *spool.RestoreReport, now time.Time) {
	fmt.Fprintf(w, "Restored %s from %s\n", rep.To, rep.From)
	fmt.Fprintf(w, "  recovery point   %s, %s ago (the mirror's last complete pass)\n",
		rep.RecoveryPoint.Format(time.RFC3339), now.Sub(rep.RecoveryPoint).Round(time.Second))
	if rep.Identity != "" {
		fmt.Fprintf(w, "  metering source  %s\n", rep.Identity)
	}
	for _, l := range rep.Ledgers {
		if l.Records == 0 {
			fmt.Fprintf(w, "  %-16s no records retained, last committed seq %d, verified\n", l.Dir, l.LastSeq)
		} else {
			fmt.Fprintf(w, "  %-16s seq %d-%d, %d records in %d segment(s), verified; last committed seq %d written %s\n",
				l.Dir, l.FirstSeq, l.LastSeq, l.Records, l.Segments, l.LastSeq, l.LastAt.Format(time.RFC3339))
		}
		for _, source := range sortedKeys(l.Watermarks) {
			fmt.Fprintf(w, "    watermark %-20s %s\n", source, l.Watermarks[source].UTC().Format(time.RFC3339))
		}
		for _, dest := range sortedKeys(l.Cursors) {
			c := l.Cursors[dest]
			note := "in step"
			if c < l.LastSeq {
				note = fmt.Sprintf("%d record(s) to send", l.LastSeq-c)
			}
			if was, ok := l.Clamped[dest]; ok {
				note += fmt.Sprintf("; the mirror held %d, beyond the restored ledger, and it was lowered", was)
			}
			fmt.Fprintf(w, "    cursor %-23s %d (%s)\n", dest, c, note)
		}
	}
	fmt.Fprintf(w, "  state            %d file(s), %d bytes: held events, dead letters, counter baselines, pause spans, owner history, adapter and billing states, tokens\n",
		rep.StateFiles, rep.StateBytes)
	if r := rep.Replaced; r != nil {
		fmt.Fprintf(w, "  replaced         the ledger that was in %s (last seq %d)\n", r.Dir, r.LastSeq)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "  note             %s\n", n)
	}
	backfill := config.Load().MaxBackfill.Round(time.Minute).String()
	if h := config.Load().MaxBackfill.Hours(); h == float64(int(h)) && h >= 1 {
		backfill = fmt.Sprintf("%d hours", int(h))
	}
	fmt.Fprintf(w, `
What vBilling does when it starts on %s:
  - It continues the restored ledger: the next record links to seq %d.
  - It meters every window since the watermarks again, from the lifetimes of
    the objects it finds, up to %s back. Windows older than that are not
    metered.
  - It sends each destination everything after its cursor. What a billing
    platform already received is sent again under the same event ID and
    deduplicated there.
  - Prometheus counters continue from the restored baselines: the increase
    since the recovery point is billed once, in the first window read.
  - Held events, dead letters, pause spans and billing states are as of the
    recovery point.
Not restored: usage pushed to vBilling after the recovery point (the ingest
API, per-request records). Senders that retry, or request logs that vBilling
pulls, fill it in.
`, rep.To, rep.Ledgers[0].LastSeq, backfill)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func ledgerExport(args []string, stdout, stderr io.Writer) int {
	fs, dir, archive := ledgerFlags("export", `Usage: vbilling ledger export --from TIME --to TIME [--tenant ID] [--metric CODE] [--dir DIR] [--archive DIR]

Writes the committed usage events whose window overlaps [--from, --to) as
NDJSON to stdout, from the archive and the live ledger, in ledger order.
Times are RFC 3339 or YYYY-MM-DD (UTC). Run "vbilling ledger verify" first:
export checks each event's hash, not the whole chain.

`, stderr)
	fromFlag := fs.String("from", "", "start of the range, inclusive")
	toFlag := fs.String("to", "", "end of the range, exclusive")
	tenant := fs.String("tenant", "", "only events of this tenant")
	metric := fs.String("metric", "", "only events of this metric")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *dir == "" && *archive == "" {
		fmt.Fprintln(stderr, "set --dir, --archive or both")
		return 2
	}
	if *fromFlag == "" || *toFlag == "" {
		fmt.Fprintln(stderr, "--from and --to are required")
		return 2
	}
	from, err := parseLedgerTime(*fromFlag)
	if err != nil {
		fmt.Fprintf(stderr, "--from: %v\n", err)
		return 2
	}
	to, err := parseLedgerTime(*toFlag)
	if err != nil {
		fmt.Fprintf(stderr, "--to: %v\n", err)
		return 2
	}
	if !to.After(from) {
		fmt.Fprintln(stderr, "--to must be after --from")
		return 2
	}

	out := bufio.NewWriterSize(stdout, 1<<16)
	enc := json.NewEncoder(out)
	n := 0
	sum, err := spool.ScanLedger(*dir, *archive, from, to, func(e *usage.Event) error {
		if (*tenant != "" && e.Tenant != *tenant) || (*metric != "" && e.Metric != *metric) {
			return nil
		}
		n++
		return enc.Encode(e)
	})
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(stderr, "export failed after %d event(s): %v\n", n, err)
		return 1
	}
	fmt.Fprintf(stderr, "exported %d event(s) from retained records %d-%d\n", n, sum.FirstSeq, sum.LastSeq)
	if sum.Anchor != "" && from.Before(sum.FirstAt) {
		fmt.Fprintf(stderr, "warning: the oldest retained record was written at %s, after --from; usage recorded before then was deleted before archiving was enabled\n",
			sum.FirstAt.UTC().Format(time.RFC3339))
	}
	return 0
}

// parseLedgerTime reads RFC 3339 or YYYY-MM-DD (UTC), like the HTTP API.
func parseLedgerTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC 3339 or YYYY-MM-DD)", v)
}
