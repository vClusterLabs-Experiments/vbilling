package spool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestDeadLetterAfterTornTailSurvivesRestartAndSelectiveReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadletter-stripe.log")
	if err := os.WriteFile(path, []byte(`{"event":`), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"delivered", "still-pending"} {
		if err := s.AddDeadLetter("stripe", usage.Event{ID: id}, "review needed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveDeadLetters("stripe", []string{"delivered"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dls, err := s.DeadLetters("stripe")
	if err != nil || len(dls) != 1 || dls[0].Event.ID != "still-pending" {
		t.Fatalf("pending dead letter lost: %+v %v", dls, err)
	}
}
