package buildinfo

import (
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildinfoReadsVersionFallback(t *testing.T) {
	info := Read("")
	if info.GoVersion == "" {
		t.Fatal("GoVersion is empty")
	}
	if info.Version == "" {
		t.Fatal("Version is empty; the fallback must be dev or a module version")
	}
}

func TestBuildinfoPrefersTheLinkerVersion(t *testing.T) {
	if got := Read("v9.9.9").Version; got != "v9.9.9" {
		t.Fatalf("Version = %q, want the ldflags value", got)
	}
}

// A build that carries no VCS settings (go run, go test, a build outside a
// checkout) never looked at the tree, so it must not say the tree was clean.
// git_modified false is a statement, and absent is the honest answer.
func TestInfoOmitsGitModifiedWhenTheTreeStateIsUnknown(t *testing.T) {
	encoded, err := json.Marshal(Info{Version: "dev", GoVersion: "go1.27.3"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "git_modified") {
		t.Errorf("an Info with no VCS answer asserts one: %s", encoded)
	}
}

// When the toolchain did record the tree state, the answer survives a round trip
// either way. The committed reports carry "git_modified": false and must keep
// decoding as a recorded false.
func TestInfoKeepsGitModifiedWhenTheToolchainRecordedIt(t *testing.T) {
	for _, modified := range []string{"true", "false"} {
		t.Run(modified, func(t *testing.T) {
			raw := `{"twinwright_version":"v1.0.0","git_revision":"abc123","git_modified":` + modified + `,"go_version":"go1.27.3"}`
			var info Info
			if err := json.Unmarshal([]byte(raw), &info); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			if want := `"git_modified":` + modified; !strings.Contains(string(encoded), want) {
				t.Errorf("round trip lost the recorded tree state, want %s in %s", want, encoded)
			}
		})
	}
}

// Read reports the tree state only when the toolchain stamped one. The test
// binary is built without VCS settings, which is the case under test; a build
// that does carry them has nothing to prove here.
func TestReadLeavesTheTreeStateUnknownWithoutVCSSettings(t *testing.T) {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info in this binary")
	}
	for _, setting := range build.Settings {
		if strings.HasPrefix(setting.Key, "vcs.") {
			t.Skipf("this test binary carries %s, so there is no unstamped build to read", setting.Key)
		}
	}
	info := Read("")
	if info.Revision != "" {
		t.Fatalf("Revision = %q from a binary with no VCS settings", info.Revision)
	}
	if info.Modified != nil {
		t.Errorf("Modified = %v from a binary with no VCS settings, want unknown (nil)", *info.Modified)
	}
}
