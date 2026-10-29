//go:build unix

package agent

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A cassette holds the prompts and answers of a run. The 0600 mode applies when
// the file is created, and --record-cassette replaces an existing file in place,
// so a file that was 0644 stayed 0644 and a re-recording was readable by every
// user on the machine.
func TestRecordingTransportReplacesWithMode0600(t *testing.T) {
	for _, existing := range []os.FileMode{0o644, 0o666, 0o640, 0o600, 0o200} {
		t.Run(existing.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.cassette")
			if err := os.WriteFile(path, []byte("an earlier recording\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, existing); err != nil {
				t.Fatal(err)
			}
			rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
			if err != nil {
				t.Fatal(err)
			}
			if err = rec.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("a replaced cassette has mode %v, want 0600", info.Mode().Perm())
			}
			if data, _ := os.ReadFile(path); len(data) == 0 || string(data) == "an earlier recording\n" {
				t.Fatalf("the earlier recording was not replaced: %q", data)
			}
		})
	}
	t.Run("a new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "new.cassette")
		rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
		if err != nil {
			t.Fatal(err)
		}
		rec.Close()
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("a new cassette has mode %v (%v), want 0600", info.Mode().Perm(), err)
		}
	})
}
