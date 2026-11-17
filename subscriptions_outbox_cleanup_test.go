package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const legacyExpiredAsyncJournalIDs = `SELECT id FROM app_async_event_journal WHERE created_at<? ORDER BY id LIMIT ?`

func seedCleanupJournal(tb testing.TB, db *sql.DB, live, expired int, cutoff int64) {
	tb.Helper()
	// Put live rows first: an ID-ordered scan must traverse them all to find
	// the small expired tail. Payload size also models real journal pages.
	for _, group := range []struct {
		count int
		at    int64
	}{{live, cutoff + 10}, {expired, cutoff - 10}} {
		if group.count == 0 {
			continue
		}
		_, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
 INSERT INTO app_async_event_journal(event_key,source_install_id,project_id,event_json,created_at)
 SELECT printf('%d:%d',?,x),1,'test',printf('%0256d',x),? FROM n`, group.count, group.at, group.at)
		if err != nil {
			tb.Fatal(err)
		}
	}
}

func selectCleanupIDs(tb testing.TB, db *sql.DB, query string, cutoff int64) []int64 {
	tb.Helper()
	rows, err := db.Query(query, cutoff, asyncJournalCleanupBatchSize)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			tb.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return ids
}

func TestAsyncJournalCleanupUsesExpiryIndex(t *testing.T) {
	s := newTestStore(t)
	const cutoff = 1000
	seedCleanupJournal(t, s.db, 200000, 17, cutoff)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN DELETE FROM app_async_event_journal WHERE id IN ("+expiredAsyncJournalIDs+")", cutoff, asyncJournalCleanupBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH app_async_event_journal USING COVERING INDEX idx_async_journal_expiry") || strings.Contains(plan, "SCAN app_async_event_journal") || strings.Contains(plan, "TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("cleanup must use indexed selection without a full scan or sort:\n%s", plan)
	}
	oldStart := time.Now()
	oldIDs := selectCleanupIDs(t, s.db, legacyExpiredAsyncJournalIDs, cutoff)
	oldTime := time.Since(oldStart)
	newStart := time.Now()
	newIDs := selectCleanupIDs(t, s.db, expiredAsyncJournalIDs, cutoff)
	newTime := time.Since(newStart)
	if !slices.Equal(oldIDs, newIDs) || len(newIDs) != 17 {
		t.Fatalf("selection changed: old=%v new=%v", oldIDs, newIDs)
	}
	t.Logf("200,000 live rows / 17 expired: legacy=%s indexed=%s (%.1fx); plan:\n%s", oldTime, newTime, float64(oldTime)/float64(newTime), plan)
	for _, want := range []int64{17, 0} {
		deleted, err := s.cleanupAsyncEventJournal(context.Background(), cutoff)
		if err != nil || deleted != want {
			t.Fatalf("deleted=%d want=%d err=%v", deleted, want, err)
		}
	}
	var remaining int
	if err := s.db.QueryRow("SELECT count(*) FROM app_async_event_journal").Scan(&remaining); err != nil || remaining != 200000 {
		t.Fatalf("live events changed: %d %v", remaining, err)
	}
}

func TestAsyncJournalCleanupBoundedAndOldestFirst(t *testing.T) {
	s := newTestStore(t)
	const cutoff = 1000
	seedCleanupJournal(t, s.db, 3, asyncJournalCleanupBatchSize+13, cutoff)
	// This newer ID must be deleted first because it has the oldest date.
	if _, err := s.db.Exec(`INSERT INTO app_async_event_journal(event_key,source_install_id,project_id,event_json,created_at) VALUES('oldest',1,'test','{}',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO app_async_event_journal(event_key,source_install_id,project_id,event_json,created_at) VALUES('boundary',1,'test','{}',?)`, cutoff); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.cleanupAsyncEventJournal(ctx, cutoff); err == nil {
		t.Fatal("canceled cleanup succeeded")
	}
	deleted, err := s.cleanupAsyncEventJournal(context.Background(), cutoff)
	if err != nil || deleted != asyncJournalCleanupBatchSize {
		t.Fatalf("unbounded cleanup: %d %v", deleted, err)
	}
	var oldest, boundary, expired int
	if err := s.db.QueryRow(`SELECT count(*) FROM app_async_event_journal WHERE event_key='oldest'`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM app_async_event_journal WHERE event_key='boundary'`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM app_async_event_journal WHERE created_at<?`, cutoff).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if oldest != 0 || boundary != 1 || expired != 14 {
		t.Fatalf("oldest=%d boundary=%d expired=%d", oldest, boundary, expired)
	}
}

func TestAsyncJournalCleanupConcurrentAuthenticationAndEvents(t *testing.T) {
	s := newTestServer(t)
	u, err := s.store.CreateUser("concurrent@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.CreateAPIKey(u.ID, "concurrent", HashAPIKey("concurrent"), "test")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-asyncReplayRetention).Unix()
	seedCleanupJournal(t, s.store.db, 200000, asyncJournalCleanupBatchSize+100, cutoff)
	start := make(chan struct{})
	errors := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		for i := 0; i < 3; i++ {
			if _, err := s.store.cleanupAsyncEventJournal(context.Background(), cutoff); err != nil {
				errors <- err
				return
			}
		}
	})
	wg.Go(func() {
		<-start
		for i := 0; i < 100; i++ {
			if _, _, err := s.store.getPrivateAPIKeyPrincipal(HashAPIKey("concurrent")); err != nil {
				errors <- err
				return
			}
		}
	})
	wg.Go(func() {
		<-start
		for i := 0; i < 50; i++ {
			if err := s.queueAppSubscriptions(AppEvent{App: "test", InstallID: 1, Topic: "test.created"}); err != nil {
				errors <- err
				return
			}
		}
	})
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	var expired, events int
	if err := s.store.db.QueryRow("SELECT count(*) FROM app_async_event_journal WHERE created_at<?", cutoff).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.QueryRow("SELECT count(*) FROM app_async_event_journal WHERE created_at> ?", cutoff+10).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || events != 50 {
		t.Fatalf("expired=%d published=%d", expired, events)
	}
}

func BenchmarkAsyncJournalCleanup(b *testing.B) {
	for _, expired := range []int{0, 1548} {
		name := "none-expired"
		if expired > 0 {
			name = "1548-expired"
		}
		b.Run(name, func(b *testing.B) {
			s, err := NewStore(filepath.Join(b.TempDir(), "benchmark.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			seedCleanupJournal(b, s.db, 1900000, expired, 1000)
			for _, q := range []struct{ name, sql string }{{"legacy", legacyExpiredAsyncJournalIDs}, {"indexed", expiredAsyncJournalIDs}} {
				b.Run("select/"+q.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						selectCleanupIDs(b, s.db, q.sql, 1000)
					}
				})
			}
			for _, mode := range []string{"legacy", "indexed"} {
				b.Run("delete/"+mode, func(b *testing.B) {
					// Restore the same expired tail before each timed deletion.
					if _, err := s.cleanupAsyncEventJournal(context.Background(), 1000); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					for b.Loop() {
						b.StopTimer()
						seedCleanupJournal(b, s.db, 0, expired, 1000)
						b.StartTimer()
						var deleted int64
						var err error
						if mode == "indexed" {
							deleted, err = s.cleanupAsyncEventJournal(context.Background(), 1000)
						} else {
							var result sql.Result
							result, err = s.db.Exec("DELETE FROM app_async_event_journal WHERE id IN ("+legacyExpiredAsyncJournalIDs+")", 1000, asyncJournalCleanupBatchSize)
							if err == nil {
								deleted, err = result.RowsAffected()
							}
						}
						if err != nil || deleted != int64(expired) {
							b.Fatalf("deleted=%d expected=%d err=%v", deleted, expired, err)
						}
					}
				})
			}
		})
	}
}
