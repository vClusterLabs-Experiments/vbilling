package diskspace

import (
	"errors"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

func TestFullVolumeFailsItsSource(t *testing.T) {
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	defer func(f func(string) (Usage, error)) { statfs = f }(statfs)
	statfs = func(path string) (Usage, error) {
		switch path {
		case "/data":
			return Usage{Total: 100, Free: 10}, nil // 90% used
		case "/archive":
			return Usage{Total: 100, Free: 60}, nil
		}
		return Usage{}, errors.New("no such volume")
	}
	Check(map[string]string{"ledger": "/data", "archive": "/archive", "gone": "/missing", "unset": ""}, h, nil)
	got := map[string]health.Status{}
	for _, s := range h.List() {
		got[s.Name] = s.Status
	}
	if got["ledger"] != health.Failing || got["archive"] != health.OK || got["gone"] != health.Failing {
		t.Fatalf("statuses %v", got)
	}
	if _, ok := got["unset"]; ok {
		t.Fatal("an unset volume must not be reported")
	}
}

func TestStatfsReadsARealVolume(t *testing.T) {
	u, err := statfs(t.TempDir())
	if err != nil || u.Total == 0 || u.Free > u.Total {
		t.Fatalf("statfs: %+v %v", u, err)
	}
}
