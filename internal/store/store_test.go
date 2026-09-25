package store

import (
 "context"
 "testing"
)

func TestSeedReproducesInitialWorld(t *testing.T) {
 snapshot := func(seed int64) string {
  t.Helper()
  s, err := Open(t.TempDir()+"/world.db"); if err != nil { t.Fatal(err) }; defer s.Close()
  w, err := s.Seed(context.Background(), seed, "manifest-digest"); if err != nil { t.Fatal(err) }
  got, err := s.Snapshot(context.Background(), w.ID); if err != nil { t.Fatal(err) }
  return got
 }
 if snapshot(42) != snapshot(42) { t.Fatal("same seed produced different state") }
 if snapshot(42) == snapshot(43) { t.Fatal("different seeds produced identical state") }
}

func TestLedgerOrderAndStableIDs(t *testing.T) {
 s, err := Open(t.TempDir()+"/world.db"); if err != nil { t.Fatal(err) }; defer s.Close()
 ctx := context.Background()
 w, err := s.Seed(ctx, 42, "digest"); if err != nil { t.Fatal(err) }
 run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "scripted", "task", ""); if err != nil { t.Fatal(err) }
 if err := s.Append(ctx, run.ID, "model.request", map[string]any{"step": 1}); err != nil { t.Fatal(err) }
 if err := s.Append(ctx, run.ID, "model.response", map[string]any{"step": 1}); err != nil { t.Fatal(err) }
 events, err := s.Events(ctx, run.ID); if err != nil { t.Fatal(err) }
 if len(events) != 3 || events[0].Type != "execution.started" || events[1].ID != run.ID+"/2" || events[2].Seq != 3 { t.Fatalf("events=%+v", events) }
}
