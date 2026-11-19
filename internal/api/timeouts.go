package api

import (
	"net/http"
	"time"
)

// serverTimeouts bound how long the API server spends on one connection.
// They are constants, not settings (Server.timeouts is replaced only by
// tests).
type serverTimeouts struct {
	// readHeader bounds reading a request's headers, and read the whole
	// request: the largest body (16 MB of records) at about 150 KB/s.
	readHeader, read time.Duration
	// write bounds answering a request, from the end of its headers: every
	// route answers small or fast (ingest, webhooks, health, the
	// dashboard's page). Routes that read the whole ledger or call billing
	// platforms set long instead (longRoute), and the raw export moves its
	// deadline with every row it writes (rollingDeadline), so an export of
	// any size streams while a client that stops reading is cut off.
	write, long time.Duration
	// idle closes keep-alive connections that send nothing.
	idle time.Duration
}

var defaultTimeouts = serverTimeouts{
	readHeader: 10 * time.Second,
	read:       2 * time.Minute,
	write:      2 * time.Minute,
	long:       30 * time.Minute,
	idle:       2 * time.Minute,
}

// HTTPServer returns the API's HTTP server on addr, with its timeouts.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.timeouts.readHeader,
		ReadTimeout:       s.timeouts.read,
		WriteTimeout:      s.timeouts.write,
		IdleTimeout:       s.timeouts.idle,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}

// longRoute gives a route that reads the whole ledger or calls billing
// platforms (usage, reconciliation, verification, price checks, replays)
// the long write deadline instead of the server's.
func (s *Server) longRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.timeouts.long))
		h(w, r)
	}
}

// rollingDeadline returns a function that keeps a streamed answer's write
// deadline the write timeout ahead of now. Call it for every row: a large
// export keeps streaming for as long as the client reads, and one that
// stops reading is cut off within the write timeout.
func (s *Server) rollingDeadline(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	var last time.Time
	return func() {
		if now := time.Now(); now.Sub(last) >= s.timeouts.write/4 {
			last = now
			_ = rc.SetWriteDeadline(now.Add(s.timeouts.write))
		}
	}
}
