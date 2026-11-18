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
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

const ledgerUsage = `Usage: vbilling ledger <command> [flags]

Read-only access to the usage ledger and its long-term archive. Safe to run
next to a live vBilling (kubectl exec) or on a copy of the files.

Commands:
  verify   check archived and live records as one hash chain (JSON report)
  export   write the usage events of a time range as NDJSON

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

`, stderr)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *dir == "" && *archive == "" {
		fmt.Fprintln(stderr, "set --dir, --archive or both")
		return 2
	}
	rep := spool.VerifyChain(*dir, *archive)
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !rep.OK {
		return 1
	}
	return 0
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
