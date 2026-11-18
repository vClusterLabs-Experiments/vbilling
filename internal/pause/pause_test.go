package pause

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func TestSpansOpenCloseAndSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Begin("c1", t0, "vbilling"); !ok {
		t.Fatal("first Begin did not open a span")
	}
	if ok, _ := s.Begin("c1", t0.Add(time.Minute), "external"); ok {
		t.Fatal("Begin opened a second span while one is open")
	}
	if ok, _ := s.End("c1", t0.Add(10*time.Minute)); !ok {
		t.Fatal("End did not close the span")
	}
	if ok, _ := s.End("c1", t0.Add(11*time.Minute)); ok {
		t.Fatal("End closed a span twice")
	}
	// A pause that starts before the last one ended never overlaps it.
	_, _ = s.Begin("c1", t0.Add(5*time.Minute), "vbilling")
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Spans("c1")
	if len(got) != 2 || !got[1].From.Equal(t0.Add(10*time.Minute)) || !got[1].Open() || got[0].By != "vbilling" {
		t.Fatalf("spans after restart = %+v", got)
	}
	if sp, open := s2.Current("c1"); !open || sp.From != got[1].From {
		t.Fatalf("current = %+v %t", sp, open)
	}
	if err := s2.Prune(t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := s2.Spans("c1"); len(got) != 1 || !got[0].Open() {
		t.Fatalf("prune kept %+v, want only the open span", got)
	}
}

func TestOverlap(t *testing.T) {
	spans := []Span{{From: t0.Add(10 * time.Second), To: t0.Add(20 * time.Second)}, {From: t0.Add(50 * time.Second)}}
	ws, we := t0, t0.Add(time.Minute)
	if d := Overlap(spans, time.Time{}, time.Time{}, ws, we); d != 20*time.Second {
		t.Fatalf("overlap = %s, want 20s (10s closed + 10s open)", d)
	}
	// Bounded by the resource's own lifetime.
	if d := Overlap(spans, t0.Add(15*time.Second), t0.Add(55*time.Second), ws, we); d != 10*time.Second {
		t.Fatalf("overlap within lifetime = %s, want 10s", d)
	}
	if d := Overlap(nil, time.Time{}, time.Time{}, ws, we); d != 0 {
		t.Fatal("no spans, some overlap")
	}
}

func TestParseKeepAndInventory(t *testing.T) {
	k, err := ParseKeep(DefaultKeep)
	if err != nil || !k[KeepStorage] || !k[KeepLoadBalancers] || k[KeepNodes] {
		t.Fatalf("default keep = %v, %v", k, err)
	}
	if k, err := ParseKeep("none"); err != nil || len(k) != 0 || k.String() != "none" {
		t.Fatalf("none = %v, %v", k, err)
	}
	if _, err := ParseKeep("storage,gpus"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for _, s := range []*Store{Memory(), must(Open(t.TempDir()))} {
		if err := s.SaveInventory("vcluster-ns/a", []byte(`{"nodes":[]}`)); err != nil {
			t.Fatal(err)
		}
		if b, ok := s.Inventory("vcluster-ns/a"); !ok || string(b) != `{"nodes":[]}` {
			t.Fatalf("inventory = %q %t", b, ok)
		}
		if len(s.All()) != 0 {
			t.Fatalf("an inventory showed up as spans: %v", s.All())
		}
		if err := s.DropInventory("vcluster-ns/a"); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.Inventory("vcluster-ns/a"); ok {
			t.Fatal("inventory still there")
		}
	}
}

func must(s *Store, err error) *Store {
	if err != nil {
		panic(err)
	}
	return s
}
