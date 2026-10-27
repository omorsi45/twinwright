package buildinfo

import "testing"

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
