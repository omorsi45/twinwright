package bench

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"twinwright/internal/buildinfo"
)

func standardSuite(t *testing.T, root string) Suite {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "bench", "standard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func TestReportCarriesSchemaVersionAndEnvironment(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	seed := int64(7)
	report, err := Run(context.Background(), standardSuite(t, root), Options{
		ExamplesRoot: root,
		WorkDir:      t.TempDir(),
		Agent:        "scripted",
		Seed:         &seed,
		ProviderFor:  scriptedProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 2 || ReportSchemaVersion != 2 {
		t.Fatalf("SchemaVersion = %d, constant = %d, want 2", report.SchemaVersion, ReportSchemaVersion)
	}
	env := report.Environment
	if env == nil {
		t.Fatal("report has no environment")
	}
	if env.Seed != 7 {
		t.Fatalf("Seed = %d, want 7", env.Seed)
	}
	if env.GOOS != runtime.GOOS || env.GOARCH != runtime.GOARCH {
		t.Fatalf("platform = %s/%s", env.GOOS, env.GOARCH)
	}
	if env.NumCPU <= 0 {
		t.Fatalf("NumCPU = %d", env.NumCPU)
	}
	started, err := time.Parse(time.RFC3339, env.StartedAt)
	if err != nil {
		t.Fatalf("StartedAt %q is not RFC 3339: %v", env.StartedAt, err)
	}
	if _, offset := started.Zone(); offset != 0 {
		t.Fatalf("StartedAt %q is not UTC", env.StartedAt)
	}
	if report.ContentDigest == "" {
		t.Fatal("ContentDigest is empty")
	}
}

func TestReportEnvironmentRecordsTheBuildOption(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	suite := standardSuite(t, root)
	suite.Cases = suite.Cases[:1]
	report, err := Run(context.Background(), suite, Options{
		ExamplesRoot: root,
		WorkDir:      t.TempDir(),
		Agent:        "scripted",
		Build:        buildinfo.Info{Version: "v1.2.3", GoVersion: "go-test"},
		ProviderFor:  scriptedProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Environment.Version; got != "v1.2.3" {
		t.Fatalf("Version = %q, want the Options.Build value", got)
	}
	if report.Environment.Seed != 42 {
		t.Fatalf("default Seed = %d, want 42", report.Environment.Seed)
	}
}

func TestContentDigestCoversFixtureBytes(t *testing.T) {
	source := filepath.Join("..", "..", "examples")
	root := filepath.Join(t.TempDir(), "examples")
	if err := os.CopyFS(root, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	suite := standardSuite(t, root)
	beforeDigest := suite.Digest()
	before, err := suite.ContentDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := suite.ContentDigest(root)
	if err != nil || again != before {
		t.Fatalf("ContentDigest unstable: %q vs %q (%v)", before, again, err)
	}

	var assertionPath string
	for _, c := range suite.Cases {
		if c.Assertions != "" {
			assertionPath = filepath.Join(root, filepath.FromSlash(c.Assertions))
			break
		}
	}
	if assertionPath == "" {
		t.Fatal("standard suite references no assertion file")
	}
	f, err := os.OpenFile(assertionPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("#"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if suite.Digest() != beforeDigest {
		t.Fatal("Digest changed with no suite edit")
	}
	after, err := suite.ContentDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("ContentDigest ignored a changed fixture byte")
	}
}

func TestContentDigestFailsOnAnUnreadableFixture(t *testing.T) {
	root := t.TempDir()
	suite := Suite{Version: 1, Name: "s", Cases: []Case{{ID: "a", Assertions: "missing.yaml"}}}
	if _, err := suite.ContentDigest(root); err == nil {
		t.Fatal("ContentDigest hashed a suite whose fixture does not exist")
	}
}

func TestReferencedFilesAreSortedAndDeduplicated(t *testing.T) {
	suite := Suite{Version: 1, Name: "s", Cases: []Case{
		{ID: "a", Chaos: "z.yaml", Assertions: "a.yaml"},
		{ID: "b", Auth: "m.yaml", Assertions: "a.yaml", Interventions: "i.yaml"},
		{ID: "c"},
	}}
	got := suite.referencedFiles()
	want := []string{"a.yaml", "i.yaml", "m.yaml", "z.yaml"}
	if len(got) != len(want) {
		t.Fatalf("referencedFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("referencedFiles = %v, want %v", got, want)
		}
	}
}

func TestDecodeReportTreatsMissingSchemaVersionAsOne(t *testing.T) {
	report, err := DecodeReport([]byte(`{"suite":"s","cases":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", report.SchemaVersion)
	}
	if report.Environment != nil {
		t.Fatal("a v1 report must decode with no environment")
	}
}

func TestDecodeReportKeepsAnExplicitSchemaVersion(t *testing.T) {
	report, err := DecodeReport([]byte(`{"schema_version":2,"suite":"s","cases":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 2 {
		t.Fatalf("SchemaVersion = %d, want 2", report.SchemaVersion)
	}
}
