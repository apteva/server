package sshwatch

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStoreDedupeCursorAndPrune(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	st, err := OpenStore(path, true)
	if err != nil {
		t.Fatal(err)
	}
	policy := mustPolicy(t, Policy{})
	now := time.Now().UTC()
	ev := Event{ID: "a", MachineID: "m", Kind: KindSSHAuth, Result: ResultAccepted, User: "root", SourceIP: "203.0.113.1", Method: "password", ObservedAt: now}
	var first, second bool
	if err := st.Update(ctx, func(tx *Tx) error {
		dup := ev
		var err error
		if first, err = tx.InsertEvent(&ev, policy); err != nil {
			return err
		}
		if second, err = tx.InsertEvent(&dup, policy); err != nil {
			return err
		}
		return tx.SetCursor("fake", "c1")
	}); err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("dedupe: first=%v second=%v", first, second)
	}
	if ev.Seq < 1 || len(ev.Alerts) == 0 {
		t.Fatalf("seq/alerts not assigned: %+v", ev)
	}
	// A failed transaction leaves neither the event nor the cursor behind.
	_ = st.Update(ctx, func(tx *Tx) error {
		e := Event{ID: "b", Kind: KindSSHAuth, ObservedAt: now}
		if _, err := tx.InsertEvent(&e, policy); err != nil {
			return err
		}
		if err := tx.SetCursor("fake", "c2"); err != nil {
			return err
		}
		return context.Canceled
	})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = OpenStore(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if c, _ := st.Cursor(ctx, "fake"); c != "c1" {
		t.Fatalf("cursor after reopen = %q", c)
	}
	page, err := st.Events(ctx, 0, 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != "a" || page.NextAfter != page.Events[0].Seq {
		t.Fatalf("events %+v %v", page, err)
	}
	if page, _ := st.Events(ctx, page.NextAfter, 10); len(page.Events) != 0 || page.NextAfter != ev.Seq {
		t.Fatalf("paging past end: %+v", page)
	}
	if _, err := OpenStore(filepath.Join(t.TempDir(), "missing.db"), false); err == nil {
		t.Fatal("reader created a store")
	}

	// Retention: age first, then row cap (oldest first).
	if err := st.Update(ctx, func(tx *Tx) error {
		old := Event{ID: "old", Kind: KindSSHSession, ObservedAt: now.Add(-400 * 24 * time.Hour)}
		if _, err := tx.InsertEvent(&old, policy); err != nil {
			return err
		}
		for i := 0; i < 5; i++ {
			e := Event{ID: "n" + strconv.Itoa(i), Kind: KindSSHSession, ObservedAt: now}
			if _, err := tx.InsertEvent(&e, policy); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Prune(ctx, now, 365*24*time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	page, _ = st.Events(ctx, 0, 10)
	if len(page.Events) != 3 || page.Events[0].ID != "n2" {
		t.Fatalf("prune kept %+v", page.Events)
	}
	stats, _ := st.stats(ctx)
	if stats.backlog != 0 {
		t.Fatalf("outbox rows of pruned events kept: %d", stats.backlog)
	}
}
