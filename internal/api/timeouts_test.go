package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Fourth review, finding 28: the API server had no read, write or idle
// timeouts, so a client that sends or reads slowly held its connection, and
// the request's memory, for as long as it liked.
func TestAPIServerHasEveryTimeout(t *testing.T) {
	srv := setup(t).server.HTTPServer(":8080")
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("timeouts: read header %s, read %s, write %s, idle %s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
	// The largest body (16 MB of records) still arrives at 150 KB/s.
	if srv.ReadTimeout < time.Duration(maxRecordsBody/(150<<10))*time.Second || srv.MaxHeaderBytes != MaxHeaderBytes || srv.Handler == nil {
		t.Fatalf("read timeout %s for a %d-byte body, header limit %d", srv.ReadTimeout, maxRecordsBody, srv.MaxHeaderBytes)
	}
}

// deadlineRecorder records the write deadlines a handler sets.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// Routes that read the whole ledger or call billing platforms answer within
// the long deadline, not the server's write timeout.
func TestLongRoutesGetTheLongWriteDeadline(t *testing.T) {
	e := setup(t)
	h := e.server.Handler()
	for _, route := range []string{"GET /api/v1/usage", "GET /api/v1/reconcile", "GET /api/v1/ledger/verify", "POST /api/v1/pricing/check",
		"POST /api/v1/destinations/fakebill/dead-letters/replay", "GET /api/v1/crosscheck"} {
		var method, path string
		fmt.Sscan(route, &method, &path)
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+e.token)
		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		start := time.Now()
		h.ServeHTTP(w, req)
		if len(w.deadlines) == 0 || w.deadlines[0].Sub(start) < defaultTimeouts.long-time.Minute {
			t.Errorf("%s: write deadlines %v, want one %s ahead", route, w.deadlines, defaultTimeouts.long)
		}
	}
}

// The raw export streams for as long as the client reads: its write
// deadline moves with every row, so an export far longer than the write
// timeout arrives whole. Both ends keep small socket buffers, as a slow link
// does: a large receive buffer opens its window only after long reads, and
// a single write would then wait longer than this test's short timeout.
// A write that blocks cannot move its own deadline, so the timeout leaves
// room for a loaded test machine to stall the reader: about 1.5s, since the
// deadline is moved at most a quarter of the timeout late.
func TestExportStreamsPastTheWriteTimeout(t *testing.T) {
	e := setup(t)
	const n = 2000
	ws := now.Add(-2 * time.Hour)
	evs := make([]usage.Event, 0, n)
	for i := 0; i < n; i++ {
		ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 1, SKU: "NVIDIA-H100", Region: "ap-southeast-2",
			WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{usage.DimTenantCluster: fmt.Sprintf("c%d", i)},
			Source: "collector", RecordedAt: now}
		ev.Finalize()
		evs = append(evs, ev)
	}
	if err := e.sp.AppendWindow(spool.SourceCollector, time.Time{}, evs, ""); err != nil {
		t.Fatal(err)
	}
	e.server.timeouts.write = 2 * time.Second
	hs := e.server.HTTPServer("")
	ts := httptest.NewUnstartedServer(hs.Handler)
	ts.Config.ReadHeaderTimeout, ts.Config.ReadTimeout, ts.Config.WriteTimeout, ts.Config.IdleTimeout = hs.ReadHeaderTimeout, hs.ReadTimeout, hs.WriteTimeout, hs.IdleTimeout
	ts.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if tc, ok := c.(*net.TCPConn); ok && s == http.StateNew {
			tc.SetWriteBuffer(16 << 10)
		}
	}
	ts.Start()
	defer ts.Close()

	var dialer net.Dialer
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetReadBuffer(16 << 10)
		}
		return c, err
	}}}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	var body bytes.Buffer
	buf := make([]byte, 8<<10)
	for {
		k, err := resp.Body.Read(buf)
		body.Write(buf[:k])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the export broke off after %s and %d bytes: %v", time.Since(start), body.Len(), err)
		}
		time.Sleep(40 * time.Millisecond) // a client that reads slowly
	}
	if took := time.Since(start); took < e.server.timeouts.write*3/2 {
		t.Fatalf("test setup: the export took %s, not longer than the write timeout %s", took, e.server.timeouts.write)
	}
	if lines := bytes.Count(body.Bytes(), []byte("\n")); lines != n {
		t.Fatalf("the export streamed %d of %d events", lines, n)
	}
}
