package release

import (
	"context"
	"testing"

	"github.com/rkingon/ecsd/internal/testutil"
)

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := &Store{API: testutil.NewSSM(), Parameter: "/shop/prod/release"}
	if r, err := s.Current(ctx); err != nil || r != nil {
		t.Fatalf("Current before any release = %v, %v", r, err)
	}
	for _, sha := range []string{"aaaaaaa1", "bbbbbbb2", "ccccccc3"} {
		if err := s.Record(ctx, &Release{SHA: sha, Kind: Deploy, Revisions: map[string]string{"api": "arn:" + sha}}); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := s.Current(ctx)
	if err != nil || cur.SHA != "ccccccc3" || cur.Version != 3 {
		t.Fatalf("Current = %+v, %v", cur, err)
	}
	h, err := s.History(ctx, 2)
	if err != nil || len(h) != 2 || h[0].SHA != "ccccccc3" || h[1].SHA != "bbbbbbb2" {
		t.Fatalf("History = %+v, %v", h, err)
	}
}

func TestRollbackTarget(t *testing.T) {
	history := []Release{
		{SHA: "c", Kind: Rollback, From: "d"},
		{SHA: "d", Kind: Deploy},
		{SHA: "c", Kind: Deploy},
		{SHA: "b", Kind: Deploy},
	}
	// Current is c, rolled back to from d: d is bad, c is current, so b.
	got, err := RollbackTarget(history, &history[0])
	if err != nil || got.SHA != "b" {
		t.Fatalf("RollbackTarget = %+v, %v", got, err)
	}
	if _, err := RollbackTarget(history[3:], &history[3]); err == nil {
		t.Error("RollbackTarget with one release: no error")
	}
}

func TestRolledBackFrom(t *testing.T) {
	history := []Release{
		{SHA: "a", Kind: Rollback, From: "b", Version: 3},
		{SHA: "b", Kind: Deploy, Version: 2},
		{SHA: "a", Kind: Deploy, Version: 1},
	}
	if r, bad := RolledBackFrom(history, "b"); !bad || r.Version != 3 {
		t.Errorf("RolledBackFrom(b) = %+v, %v", r, bad)
	}
	if _, bad := RolledBackFrom(history, "a"); bad {
		t.Error("a was never rolled back from")
	}
	// b released again (forced) after the rollback: no longer refused.
	again := append([]Release{{SHA: "b", Kind: Deploy, Version: 4}}, history...)
	if _, bad := RolledBackFrom(again, "b"); bad {
		t.Error("b, released again, still counts as rolled back from")
	}
}

func TestFind(t *testing.T) {
	history := []Release{{SHA: "abcdef123", Version: 9}, {SHA: "0123456789", Version: 8}}
	if r, err := Find(history, "0123456"); err != nil || r.Version != 8 {
		t.Errorf("Find(sha prefix) = %+v, %v", r, err)
	}
	if r, err := Find(history, "v9"); err != nil || r.SHA != "abcdef123" {
		t.Errorf("Find(v9) = %+v, %v", r, err)
	}
	if _, err := Find(history, "abc"); err == nil {
		t.Error("Find(short prefix): no error")
	}
}
