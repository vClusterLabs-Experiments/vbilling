package records

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// kong maps a Kong AI Gateway HTTP Log entry, the way docs/usage-sources.html
// shows it.
var kong = Mapping{
	ID:     Path{"request.id"},
	Time:   Path{"started_at"},
	Tenant: Path{"consumer.username"},
	Usage:  map[string]Path{"tokens_in": {"ai.proxy.usage.prompt_tokens"}, "tokens_out": {"ai.proxy.usage.completion_tokens"}},
	SKU:    Path{"ai.proxy.meta.response_model"},
}

func kongRow(id, consumer string, at time.Time, in, out int) map[string]any {
	return map[string]any{
		"request":    map[string]any{"id": id},
		"started_at": at.UTC().Format(time.RFC3339Nano),
		"consumer":   map[string]any{"username": consumer},
		"ai": map[string]any{"proxy": map[string]any{
			"usage": map[string]any{"prompt_tokens": in, "completion_tokens": out},
			"meta":  map[string]any{"response_model": "llama-3.1-8b"},
		}},
	}
}

// fakeLog serves rows by time range, in pages, the way a gateway's logs API
// does.
type fakeLog struct {
	mu       sync.Mutex
	rows     []map[string]any
	requests int
	auth     string
}

func (f *fakeLog) handler(t *testing.T, cursor bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		f.auth = r.Header.Get("Authorization")
		q := r.URL.Query()
		from, err1 := time.Parse(time.RFC3339, q.Get("from"))
		to, err2 := time.Parse(time.RFC3339, q.Get("to"))
		if err1 != nil || err2 != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		var in []map[string]any
		for _, row := range f.rows {
			at, _ := time.Parse(time.RFC3339Nano, row["started_at"].(string))
			if !at.Before(from) && at.Before(to) {
				in = append(in, row)
			}
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		start := 0
		if cursor {
			start, _ = strconv.Atoi(q.Get("cursor"))
		} else {
			page, _ := strconv.Atoi(q.Get("page"))
			start = (page - 1) * limit
		}
		end := min(start+limit, len(in))
		if start > len(in) {
			start = end
		}
		out := map[string]any{"data": in[start:end]}
		if cursor && end < len(in) {
			out["meta"] = map[string]any{"next": strconv.Itoa(end)}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

func kongRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, Options{})
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	r.in = NewIngester(r.records, Formats{"kong": kong}, Options{Window: time.Minute, Now: r.clk.now}, nil, h, nil)
	return r
}

func storedTokens(t *testing.T, r *rig) (in, out float64, rows int) {
	t.Helper()
	if err := r.records.ScanRecords(t0.Add(-2*time.Hour), t0.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		rows++
		switch ev.Metric {
		case "tokens_in":
			in += ev.Quantity
		case "tokens_out":
			out += ev.Quantity
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return in, out, rows
}

func TestPullFillsWhatThePushLostAndCountsOnce(t *testing.T) {
	for _, mode := range []string{"page", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			r := kongRig(t)
			defer r.close()
			r.clk.set(t0.Add(30 * time.Minute))
			log := &fakeLog{}
			for i := 0; i < 7; i++ {
				log.rows = append(log.rows, kongRow(fmt.Sprintf("req-%d", i), "acme", t0.Add(time.Duration(i)*time.Minute+time.Second), 100+i, 10+i))
			}
			srv := httptest.NewServer(log.handler(t, mode == "cursor"))
			defer srv.Close()

			// The gateway pushed 4 of the 7 requests; 3 were lost.
			var pushed []map[string]any
			for i, row := range log.rows {
				if i%2 == 0 {
					pushed = append(pushed, row)
				}
			}
			body, _ := json.Marshal(pushed)
			if res, err := r.in.Ingest("kong", "kong", "", body); err != nil || res.Accepted != 4 {
				t.Fatalf("push: %+v %v", res, err)
			}

			t.Setenv("RECORDS_PULL_KONG_TOKEN", "s3cret\n")
			spec := PullSpec{Name: "kong-logs", Format: "kong", URL: srv.URL + "/logs?from={{from}}&to={{to}}&limit={{limit}}",
				Headers: map[string]string{"Authorization": "Bearer ${RECORDS_PULL_KONG_TOKEN}"}, Rows: Path{"data"},
				Paging: PullPaging{Mode: mode, Limit: 3}}
			if mode == "page" {
				spec.URL += "&page={{page}}"
			} else {
				spec.URL += "&cursor={{cursor}}"
				spec.Paging.Next = Path{"meta.next"}
			}
			if err := spec.normalize(Formats{"kong": kong}); err != nil {
				t.Fatal(err)
			}
			p := NewPuller(r.in, spec, nil, nil, nil, nil)
			p.now = r.clk.now
			res, err := p.Pull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Rows != 7 || res.Accepted != 3 || res.AlreadyStored != 4 {
				t.Fatalf("pull: rows %d, new %d, already stored %d", res.Rows, res.Accepted, res.AlreadyStored)
			}
			if log.auth != "Bearer s3cret" {
				t.Fatalf("auth header %q", log.auth)
			}
			in, out, n := storedTokens(t, r)
			if in != 721 || out != 91 || n != 14 {
				t.Fatalf("stored tokens in %v out %v rows %d, want 721, 91 and 14 rows (7 requests x 2 metrics)", in, out, n)
			}
			// Pulling again adds nothing.
			if res, err := p.Pull(context.Background()); err != nil || res.Accepted != 0 {
				t.Fatalf("second pull: %+v %v", res, err)
			}
			if _, _, n := storedTokens(t, r); n != 14 {
				t.Fatalf("second pull stored more rows: %d", n)
			}
		})
	}
}

func TestPullSpecsAreChecked(t *testing.T) {
	formats := Formats{"kong": kong}
	write := func(t *testing.T, s string) string {
		p := filepath.Join(t.TempDir(), "pull.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `
- name: kong-logs
  format: kong
  url: "https://logs.example.com/requests?from={{ from }}&to={{to}}&page={{page}}"
  headers: {Authorization: "Bearer ${RECORDS_PULL_KONG_TOKEN}"}
  rows: data
  paging: {mode: page, limit: 200}
  interval: 2m
`
	specs, err := LoadPullSpecs(write(t, good), formats)
	if err != nil || len(specs) != 1 || specs[0].Client != "kong" || time.Duration(specs[0].Interval) != 2*time.Minute ||
		time.Duration(specs[0].Lookback) != time.Hour || specs[0].Paging.Start != 1 || specs[0].Method != "GET" {
		t.Fatalf("specs = %+v, %v", specs, err)
	}
	for name, bad := range map[string]string{
		"no time range":   "- {name: a, format: kong, url: 'https://x/logs'}",
		"unknown format":  "- {name: a, format: envoy, url: 'https://x/logs?from={{from}}'}",
		"other secrets":   "- {name: a, format: kong, url: 'https://x/logs?from={{from}}', headers: {Authorization: 'Bearer ${STRIPE_API_KEY}'}}",
		"cursor, no next": "- {name: a, format: kong, url: 'https://x/logs?from={{from}}', paging: {mode: cursor}}",
		"not http":        "- {name: a, format: kong, url: 'file:///etc/passwd?from={{from}}'}",
		"same name twice": "- {name: a, format: kong, url: 'https://x/l?from={{from}}'}\n- {name: a, format: kong, url: 'https://y/l?from={{from}}'}",
		"unknown field":   "- {name: a, format: kong, url: 'https://x/l?from={{from}}', rowz: data}",
		"tiny interval":   "- {name: a, format: kong, url: 'https://x/l?from={{from}}', interval: 1s}",
	} {
		if _, err := LoadPullSpecs(write(t, bad), formats); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPullFailureIsVisible(t *testing.T) {
	r := kongRig(t)
	defer r.close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	spec := PullSpec{Name: "kong-logs", Format: "kong", URL: srv.URL + "/logs?from={{from}}&to={{to}}&key=secret"}
	if err := spec.normalize(Formats{"kong": kong}); err != nil {
		t.Fatal(err)
	}
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	p := NewPuller(r.in, spec, nil, nil, h, nil)
	p.now = r.clk.now
	_, err := p.Pull(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v (a failure must be reported without the query string)", err)
	}
	if probs := h.Problems(); len(probs) != 1 || probs[0].Status != health.Failing {
		t.Fatalf("health = %+v", probs)
	}
}

// A log that redirects to another origin is still read, but the pull's
// headers (its tokens) and the referring URL stay with the configured origin.
func TestPullHeadersStayOnTheConfiguredOrigin(t *testing.T) {
	r := kongRig(t)
	defer r.close()
	t.Setenv("RECORDS_PULL_KONG_TOKEN", "kong-secret")
	var mu sync.Mutex
	var elsewhere []http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		elsewhere = append(elsewhere, req.Header.Clone())
		mu.Unlock()
		_, _ = w.Write([]byte("[]"))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+req.URL.Path+"?"+req.URL.RawQuery, http.StatusFound)
	}))
	defer origin.Close()
	spec := PullSpec{Name: "kong-logs", Format: "kong", URL: origin.URL + "/logs?from={{from}}&to={{to}}&key=q-secret",
		Headers: map[string]string{"X-Api-Key": "${RECORDS_PULL_KONG_TOKEN}", "Authorization": "Bearer ${RECORDS_PULL_KONG_TOKEN}"}}
	if err := spec.normalize(Formats{"kong": kong}); err != nil {
		t.Fatal(err)
	}
	p := NewPuller(r.in, spec, nil, nil, nil, nil)
	p.now = r.clk.now
	if _, err := p.Pull(context.Background()); err != nil {
		t.Fatalf("a redirected log must still be read: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(elsewhere) == 0 {
		t.Fatal("the redirect was not followed")
	}
	for _, hdr := range elsewhere {
		for _, k := range []string{"X-Api-Key", "Authorization", "Referer"} {
			if v := hdr.Get(k); v != "" {
				t.Fatalf("%s was sent to another origin: %q", k, v)
			}
		}
	}
}
