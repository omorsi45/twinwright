package assertion

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/chaos"
)

func checkExample(t *testing.T, f fixture, name string) Report {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "assertions", name))
	if err != nil {
		t.Fatal(err)
	}
	set, err := Parse(raw, f.manifest, Builtins())
	if err != nil {
		t.Fatal(err)
	}
	report, err := Check(context.Background(), f.store, f.run.ID, set)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func failedIDs(report Report) string {
	var ids []string
	for _, result := range report.Results {
		if !result.Passed {
			ids = append(ids, result.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestDuplicateChargeExample(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.manifest = manifestFrom(t, "billing")
	world, err := f.store.Seed(ctx, 42, f.manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	f.run = execute(t, f.store, f, agent.ScriptedProvider{}, run.ID)
	if report := checkExample(t, f, "duplicate-charge.yaml"); !report.Passed {
		t.Fatalf("failed=%s report=%+v", failedIDs(report), report)
	}
}

func TestAmbiguousCommitExample(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		ctx := context.Background()
		f := newFixture(t)
		f.manifest = manifestFrom(t, "billing")
		world, err := f.store.SeedScenario(ctx, 43, f.manifest.Digest, "ambiguous-commit")
		if err != nil {
			t.Fatal(err)
		}
		policy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := policy.CanonicalJSON()
		run, err := f.store.CreateRunWithChaos(ctx, world.ID, "ambiguous-commit", "scripted", "fixture-v1", "task", "", encoded, policy.Digest())
		if err != nil {
			t.Fatal(err)
		}
		f.run = execute(t, f.store, f, agent.AmbiguousScriptedProvider{Unsafe: unsafe}, run.ID)
		report := checkExample(t, f, "ambiguous-commit.yaml")
		want := ""
		if unsafe {
			want = "exactly_one_refund,one_500_cent_refund_on_duplicate,scenario_checks"
		}
		if got := failedIDs(report); got != want {
			t.Fatalf("unsafe=%v failed=%q want %q", unsafe, got, want)
		}
	}
}

func TestPromptInjectionExample(t *testing.T) {
	if report := checkExample(t, injectionFixture(t, "support-policy.yaml"), "prompt-injection-ticket.yaml"); !report.Passed {
		t.Fatalf("support failed=%s", failedIDs(report))
	}
	report := checkExample(t, injectionFixture(t, "overprivileged-policy.yaml"), "prompt-injection-ticket.yaml")
	if got, want := failedIDs(report), "foreign_lookup_denied,no_messages_posted,other_customer_not_posted,scenario_checks"; got != want {
		t.Fatalf("overprivileged failed=%q want %q", got, want)
	}
}
