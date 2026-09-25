package lease

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireRenewRelease(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	first, err := s.Acquire(ctx(t), "run/R-1", "worker-a", time.Minute, now)
	if err != nil || first.Token != 1 {
		t.Fatalf("%+v err=%v", first, err)
	}
	if _, err := s.Acquire(ctx(t), "run/R-1", "worker-b", time.Minute, now.Add(time.Second)); err == nil {
		t.Fatal("second owner acquired live lease")
	}
	renewed, err := s.Renew(ctx(t), "run/R-1", "worker-a", first.Token, time.Minute, now.Add(30*time.Second))
	if err != nil || !renewed.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("%+v err=%v", renewed, err)
	}
	if err := s.Release(ctx(t), "run/R-1", "worker-a", first.Token, now.Add(40*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Acquire(ctx(t), "run/R-1", "worker-b", time.Minute, now.Add(time.Minute))
	if err != nil || second.Token != 2 {
		t.Fatalf("fencing token did not advance: %+v err=%v", second, err)
	}
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	held, err := s.Acquire(ctx(t), "world/W-1", "worker-a", time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.Acquire(ctx(t), "world/W-1", "worker-b", time.Minute, now.Add(2*time.Second))
	if err != nil || reclaimed.Token != held.Token+1 || reclaimed.Owner != "worker-b" {
		t.Fatalf("%+v err=%v", reclaimed, err)
	}
	if _, err := s.Renew(ctx(t), "world/W-1", "worker-a", held.Token, time.Minute, now.Add(3*time.Second)); err == nil {
		t.Fatal("stale fencing token renewed after reclaim")
	}
}

func TestRejectsShortTTL(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Acquire(ctx(t), "x", "y", time.Millisecond, time.Now()); err == nil {
		t.Fatal("accepted")
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}
