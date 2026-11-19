package sourceid

import "testing"

func TestIdentitySurvivesRestartAndSeparatesLedgers(t *testing.T) {
	dir := t.TempDir()
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil || a != b {
		t.Fatalf("restart changed identity: %q %q %v", a, b, err)
	}
	c, err := Load(t.TempDir())
	if err != nil || c == a {
		t.Fatalf("independent ledger shares identity: %q %q %v", a, c, err)
	}
}
