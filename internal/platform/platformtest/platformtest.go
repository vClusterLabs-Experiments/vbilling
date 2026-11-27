// Package platformtest serves a fake vCluster Platform management API for
// tests: the fixture objects of internal/platform/testdata, which a test
// can change, API discovery, list reads, and watches that stream the
// changes. It can also serve a Platform that offers no watch, or refuses
// it.
package platformtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const mgmt = "/kubernetes/management/apis/management.loft.sh/v1"

// Resources the fake serves, as the Platform names them.
const (
	Projects  = "projects"
	Tenants   = "tenants"
	Instances = "virtualclusterinstances"
)

var kinds = map[string]string{Projects: "Project", Tenants: "Tenant", Instances: "VirtualClusterInstance"}

// Server is a fake Platform. Its zero value is not usable: use New.
type Server struct {
	URL string // the Platform URL (PLATFORM_URL)
	Key string // the access key it accepts

	t   testing.TB
	dir string
	srv *httptest.Server

	mu       sync.Mutex
	objs     map[string][]map[string]any
	rv       int
	noVerb   bool // discovery does not list the watch verb
	deny     bool // watch requests are refused (403)
	watchers map[string]map[chan []byte]bool
	lists    map[string]int
	watches  map[string]int
}

// New serves the fixtures in dir (internal/platform/testdata/<version>).
func New(t testing.TB, dir, key string) *Server {
	t.Helper()
	s := &Server{Key: key, t: t, dir: dir, objs: map[string][]map[string]any{}, rv: 1,
		watchers: map[string]map[chan []byte]bool{}, lists: map[string]int{}, watches: map[string]int{}}
	for res := range kinds {
		b, err := os.ReadFile(filepath.Join(dir, res+".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var l struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(b, &l); err != nil {
			t.Fatal(err)
		}
		for _, it := range l.Items {
			it["apiVersion"], it["kind"] = "management.loft.sh/v1", kinds[res]
			meta(it)["resourceVersion"] = "1"
		}
		s.objs[res] = l.Items
	}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL
	return s
}

// WithoutWatch serves a Platform whose API discovery lists no watch verb.
func (s *Server) WithoutWatch() *Server { s.mu.Lock(); s.noVerb = true; s.mu.Unlock(); return s }

// RefuseWatch makes watch requests fail with 403, as for an access key
// that may list but not watch, and ends the watches that are open.
func (s *Server) RefuseWatch(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny = v
	if v {
		for _, ws := range s.watchers {
			for ch := range ws {
				close(ch)
			}
		}
		s.watchers = map[string]map[chan []byte]bool{}
	}
}

// Lists returns how often a resource was listed.
func (s *Server) Lists(res string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.lists[res] }

// WatchRequests returns how often a watch of a resource was asked for.
func (s *Server) WatchRequests(res string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches[res]
}

// Watching returns how many watches of a resource are open.
func (s *Server) Watching(res string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers[res])
}

// Edit changes the object of a resource with the given name and sends the
// change to the open watches.
func (s *Server) Edit(res, name string, edit func(obj map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.objs[res] {
		if meta(it)["name"] == name {
			edit(it)
			s.rv++
			meta(it)["resourceVersion"] = strconv.Itoa(s.rv)
			s.send(res, "MODIFIED", it)
			return
		}
	}
	s.t.Fatalf("platformtest: no %s named %s", res, name)
}

// SetLabel sets (or with an empty value removes) a label of an object.
func SetLabel(obj map[string]any, key, value string) { setIn(obj, "labels", key, value) }

// SetAnnotation sets (or with an empty value removes) an annotation.
func SetAnnotation(obj map[string]any, key, value string) { setIn(obj, "annotations", key, value) }

func setIn(obj map[string]any, field, key, value string) {
	m, _ := meta(obj)[field].(map[string]any)
	if m == nil {
		m = map[string]any{}
		meta(obj)[field] = m
	}
	if value == "" {
		delete(m, key)
		return
	}
	m[key] = value
}

func meta(obj map[string]any) map[string]any {
	m, _ := obj["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		obj["metadata"] = m
	}
	return m
}

func (s *Server) send(res, typ string, obj map[string]any) {
	b, err := json.Marshal(map[string]any{"type": typ, "object": obj})
	if err != nil {
		s.t.Errorf("platformtest: %v", err)
		return
	}
	for ch := range s.watchers[res] {
		ch <- b
	}
}

func status(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "code": code})
}

// ServeHTTP answers the way the Platform's management API does.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Key {
		status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	switch p := r.URL.Path; {
	case p == mgmt:
		s.discovery(w)
	case p == mgmt+"/selves" && r.Method == http.MethodPost:
		s.file(w, "self.json")
	case strings.HasPrefix(p, mgmt+"/"):
		res := strings.TrimPrefix(p, mgmt+"/")
		if _, ok := kinds[res]; !ok {
			status(w, http.StatusNotFound, "NotFound")
			return
		}
		if r.URL.Query().Get("watch") == "true" || r.URL.Query().Get("watch") == "1" {
			s.watch(w, r, res)
			return
		}
		s.list(w, res)
	default:
		status(w, http.StatusNotFound, "NotFound")
	}
}

func (s *Server) file(w http.ResponseWriter, name string) {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		status(w, http.StatusNotFound, "NotFound")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (s *Server) discovery(w http.ResponseWriter) {
	b, err := os.ReadFile(filepath.Join(s.dir, "discovery.json"))
	if err != nil {
		s.t.Errorf("platformtest: %v", err)
		return
	}
	s.mu.Lock()
	noVerb := s.noVerb
	s.mu.Unlock()
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		s.t.Errorf("platformtest: %v", err)
		return
	}
	if noVerb {
		for _, r := range d["resources"].([]any) {
			res := r.(map[string]any)
			var verbs []any
			for _, v := range res["verbs"].([]any) {
				if v != "watch" {
					verbs = append(verbs, v)
				}
			}
			res["verbs"] = verbs
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

func (s *Server) list(w http.ResponseWriter, res string) {
	s.mu.Lock()
	s.lists[res]++
	items := s.objs[res]
	if items == nil {
		items = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"apiVersion": "management.loft.sh/v1", "kind": kinds[res] + "List",
		"metadata": map[string]any{"resourceVersion": strconv.Itoa(s.rv)}, "items": items})
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("platformtest: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (s *Server) watch(w http.ResponseWriter, r *http.Request, res string) {
	s.mu.Lock()
	s.watches[res]++
	noVerb, deny := s.noVerb, s.deny
	s.mu.Unlock()
	switch {
	case noVerb:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	case deny:
		status(w, http.StatusForbidden, "Forbidden")
		return
	}
	ch := make(chan []byte, 64)
	s.mu.Lock()
	if s.watchers[res] == nil {
		s.watchers[res] = map[chan []byte]bool{}
	}
	s.watchers[res][ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers[res], ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b, open := <-ch:
			if !open {
				return
			}
			_, _ = w.Write(append(b, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
