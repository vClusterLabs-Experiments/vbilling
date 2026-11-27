package version

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// root is the repository root, seen from this package's directory.
const root = "../.."

func TestUserAgentCarriesTheVersion(t *testing.T) {
	if got, want := UserAgent(), "vbilling/"+Version; got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version %q is not X.Y.Z", Version)
	}
}

// The validation of v0.3.2 found clients sending "vbilling/0.3.1" while the
// API reported 0.3.2: the version was written in four places and one release
// bumped only some of them. It is written once now. A literal such as
// "vbilling/0.3.3" anywhere else in the Go sources (tests included) is the
// same mistake coming back: use version.UserAgent() or version.Version.
func TestNoVersionLiteralOutsideThisPackage(t *testing.T) {
	literal := regexp.MustCompile(`vbilling/v?\d[\w.-]*`)
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if abs, _ := filepath.Abs(path); abs == self || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for i, line := range strings.Split(string(b), "\n") {
				if m := literal.FindString(line); m != "" {
					t.Errorf("%s:%d writes %q: take the version from internal/version (version.UserAgent(), version.Version)",
						strings.TrimPrefix(path, root+string(filepath.Separator)), i+1, m)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files under cmd and internal: the walk no longer finds the sources", scanned)
	}
}

// The binary, the chart and the default image are released together under
// one number. A release that bumps the chart and forgets Version (or the
// other way round) fails here, not in a customer's request log.
func TestVersionMatchesTheChart(t *testing.T) {
	chart := read(t, "deploy/helm/vbilling/Chart.yaml")
	for _, key := range []string{"version", "appVersion"} {
		m := regexp.MustCompile(`(?m)^` + key + `:\s*["']?([^\s"']+)`).FindStringSubmatch(chart)
		if m == nil {
			t.Fatalf("Chart.yaml has no %s", key)
		}
		if m[1] != Version {
			t.Errorf("Chart.yaml %s is %s, internal/version.Version is %s: bump them together", key, m[1], Version)
		}
	}
	m := regexp.MustCompile(`(?m)^image:\n(?:  .*\n)*?  tag:\s*["']?([^\s"']+)`).FindStringSubmatch(read(t, "deploy/helm/vbilling/values.yaml"))
	if m == nil {
		t.Fatal("values.yaml has no image.tag")
	}
	if m[1] != "v"+Version {
		t.Errorf("values.yaml image.tag is %s, internal/version.Version is %s: bump them together", m[1], Version)
	}
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
