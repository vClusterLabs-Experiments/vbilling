package main

import (
	"io"
	"log"
	"runtime"
	"sync"
	"time"
)

// releaseOnFatal makes a log.Fatal give the Lease up before the process
// exits, for as long as vBilling starts up.
//
// log.Fatal exits without running deferred calls, so a start-up error after
// the Lease was taken (a file that cannot be opened, a setting an adapter
// refuses) left the Lease held by a process that was gone: the restarted
// container waited for it to expire, about 30 seconds, before it could fail
// again or start. Every log.Fatal counts, whichever function calls it, so
// nothing has to remember to release the Lease first.
//
// It returns a function to call once start-up is over. From then on what
// the Lease guards (metering, ingest, delivery, enforcement) is running, and
// a process that dies leaves the Lease to expire as before: nothing may
// take over while this process could still be writing.
func releaseOnFatal(release func()) (startupOver func()) {
	w := &fatalWriter{out: log.Writer(), release: release}
	log.SetOutput(w)
	return func() {
		w.mu.Lock()
		w.release = nil
		w.mu.Unlock()
	}
}

// fatalWriter is the standard logger's output during start-up: it writes
// every line through, and after the line of a log.Fatal it calls release.
type fatalWriter struct {
	out     io.Writer
	mu      sync.Mutex
	release func() // nil once start-up is over, or once it ran
}

func (w *fatalWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.mu.Lock()
	release := w.release
	if release != nil && fatalCall() {
		w.release = nil
	} else {
		release = nil
	}
	w.mu.Unlock()
	if release != nil {
		// The process exits as soon as this returns. The release runs on its
		// own goroutine and is not waited for forever: the logger is locked
		// while it writes, so a release that logged would otherwise wait for
		// this very call.
		done := make(chan struct{})
		go func() { defer close(done); release() }()
		select {
		case <-done:
		case <-time.After(fatalReleaseWait):
		}
	}
	return n, err
}

// fatalReleaseWait bounds the wait for the Lease to be given up before a
// fatal exit (lease.Held.Release itself gives up after 10 seconds).
const fatalReleaseWait = 12 * time.Second

// fatalCall reports whether the line being written is a log.Fatal's: the
// standard logger writes it, then exits.
func fatalCall() bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		switch f.Function {
		case "log.Fatal", "log.Fatalf", "log.Fatalln", "log.(*Logger).Fatal", "log.(*Logger).Fatalf", "log.(*Logger).Fatalln":
			return true
		}
		if !more {
			return false
		}
	}
}
