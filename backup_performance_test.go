package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func seedBackupPayload(t testing.TB, db *sql.DB, mib int) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE backup_perf(id INTEGER PRIMARY KEY,data BLOB); CREATE TABLE backup_progress(n INTEGER); INSERT INTO backup_progress VALUES(0)`); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO backup_perf(data)VALUES(?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i := 0; i < mib*16; i++ {
		if _, err = stmt.Exec(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineBackupConcurrentWrites(t *testing.T) {
	mib := 64
	if n, _ := strconv.Atoi(os.Getenv("APTEVA_BACKUP_PERF_MIB")); n > 0 {
		mib = n
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "active.db")
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedBackupPayload(t, db, mib)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	dst := filepath.Join(dir, "copy.db")
	go func() { done <- onlineBackupPath(ctx, src, dst) }()
	var latencies []time.Duration
	for {
		select {
		case err = <-done:
			goto completed
		default:
		}
		at := time.Now()
		if _, e := db.ExecContext(ctx, `UPDATE backup_progress SET n=n+1`); e != nil {
			t.Fatal(e)
		}
		var n int
		if e := db.QueryRowContext(ctx, `SELECT n FROM backup_progress`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		latencies = append(latencies, time.Since(at))
		time.Sleep(2 * time.Millisecond)
	}
completed:
	if err != nil {
		t.Fatal(err)
	}
	if len(latencies) < 3 {
		t.Fatalf("insufficient concurrent traffic: %d operations", len(latencies))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("%d MiB: online snapshot=%s concurrent write/read pairs=%d p95=%s max=%s", mib, time.Since(start), len(latencies), latencies[(len(latencies)-1)*95/100], latencies[len(latencies)-1])
	if latencies[(len(latencies)-1)*95/100] > time.Second {
		t.Fatal("foreground traffic stalled")
	}
	if err = validateRecoveryDB(dst); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineBackupCancellationRemovesPartialFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedBackupPayload(t, db, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	dst := filepath.Join(dir, "cancelled.db")
	start := time.Now()
	err = onlineBackupPath(ctx, src, dst)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation was not prompt")
	}
	if _, err = os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("partial backup retained: %v", err)
	}
}

func TestRollbackJournalWritersAreNotHeldForWholeBackup(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rollback.db")
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedBackupPayload(t, db, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- onlineBackupPath(ctx, src, filepath.Join(dir, "copy.db")) }()
	writes := 0
	for {
		select {
		case err = <-done:
			goto complete
		default:
		}
		if _, err = db.Exec(`UPDATE backup_progress SET n=n+1`); err != nil {
			t.Fatal(err)
		}
		writes++
		time.Sleep(2 * time.Millisecond)
	}
complete:
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if writes < 3 {
		t.Fatalf("rollback writes stalled: %d completed", writes)
	}
	t.Logf("rollback-journal mode: %d foreground writes; snapshot result=%v", writes, err)
}

func TestPlatformSnapshotRejectsOverlapAndHonorsCancellation(t *testing.T) {
	s := newTestServer(t)
	platformBackupMu.Lock()
	for _, operation := range []func(http.ResponseWriter, *http.Request){s.writePlatformSnapshot, s.restorePlatformSnapshot} {
		w := httptest.NewRecorder()
		operation(w, httptest.NewRequest("POST", "/", nil))
		if w.Code != http.StatusConflict {
			t.Errorf("overlap status=%d", w.Code)
		}
	}
	platformBackupMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if w.Body.Len() != 0 {
		t.Fatal("cancelled request performed a snapshot")
	}
}

func TestBackupDiskBudget(t *testing.T) {
	for _, tt := range []struct {
		free, need int64
		ok         bool
	}{{backupDiskReserve + 100, 100, true}, {backupDiskReserve + 99, 100, false}, {backupDiskReserve - 1, 0, false}, {1 << 40, -1, false}} {
		if err := checkBackupSpace(tt.free, tt.need); (err == nil) != tt.ok {
			t.Errorf("%+v: %v", tt, err)
		}
	}
}

type backupCountingResponse struct {
	header http.Header
	bytes  int64
	first  time.Time
	cancel context.CancelFunc
}

func (w *backupCountingResponse) Header() http.Header { return w.header }
func (w *backupCountingResponse) WriteHeader(int)     {}
func (w *backupCountingResponse) Write(b []byte) (int, error) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
	w.bytes += int64(len(b))
	if w.cancel != nil {
		w.cancel()
	}
	return len(b), nil
}

func TestLargeSnapshotStreamingAndMemory(t *testing.T) {
	s := newTestServer(t)
	mib := 64
	if n, _ := strconv.Atoi(os.Getenv("APTEVA_BACKUP_PERF_MIB")); n > 0 {
		mib = n
	}
	seedBackupPayload(t, s.store.db, mib)
	w := &backupCountingResponse{header: make(http.Header)}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil))
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d MiB: first-byte=%s full snapshot=%s archive=%.1f MiB allocated=%.1f MiB", mib, w.first.Sub(start), time.Since(start), float64(w.bytes)/(1<<20), float64(allocated)/(1<<20))
	if w.bytes < int64(mib)<<19 {
		t.Fatal("missing large archive")
	}
	if allocated > 32<<20 {
		t.Fatalf("streaming used %d bytes of Go allocations", allocated)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w = &backupCountingResponse{header: make(http.Header), cancel: cancel}
	start = time.Now()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if w.bytes > 1<<20 {
		t.Fatal("continued sending after cancellation")
	}
	t.Logf("mid-stream cancellation completed in %s", time.Since(start))
}

func TestSnapshotManifestMatchesStreamedFiles(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	id := insertTestInstall(t, s, "streamed")
	root := filepath.Join(s.localApps.cacheDir, "streamed", "data", strconv.FormatInt(id, 10))
	os.MkdirAll(root, 0700)
	os.WriteFile(filepath.Join(root, "attachment.bin"), bytes.Repeat([]byte("payload"), 10000), 0600)
	mustSeedSqlite(t, filepath.Join(root, "data.sqlite"))
	if err := os.Chmod(filepath.Join(root, "data.sqlite"), 0700); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	files := readTarGz(t, w.Body.Bytes())
	var manifest recoveryManifest
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	for name, meta := range manifest.Files {
		sum := sha256.Sum256(files[name])
		if meta.SHA256 != hex.EncodeToString(sum[:]) || meta.Size != int64(len(files[name])) {
			t.Fatalf("digest mismatch for %s", name)
		}
	}
	for name, meta := range manifest.Files {
		if strings.HasSuffix(name, "data.sqlite") && meta.Mode != 0700 {
			t.Fatalf("SQLite executable bits lost: %o", meta.Mode)
		}
	}
	if len(manifest.Files) != 3 {
		t.Fatalf("missing file: %+v", manifest)
	}
}

func TestSnapshotRejectsOversizedFileBeforeStreaming(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	id := insertTestInstall(t, s, "oversized")
	root := filepath.Join(s.localApps.cacheDir, "oversized", "data", strconv.FormatInt(id, 10))
	os.MkdirAll(root, 0700)
	f, err := os.Create(filepath.Join(root, "huge"))
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxRestoreEntryBytes + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || w.Body.Len() > 4096 {
		t.Fatalf("oversized source was streamed: %d %d", w.Code, w.Body.Len())
	}
}

func TestSnapshotRejectsFileCountAndManifestBeforeStreaming(t *testing.T) {
	for _, mode := range []string{"files", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestServer(t)
			s.localApps = NewLocalSupervisor(t.TempDir())
			id := insertTestInstall(t, s, "limits")
			root := filepath.Join(s.localApps.cacheDir, "limits", "data", strconv.FormatInt(id, 10))
			os.MkdirAll(root, 0700)
			count := maxRestoreArchiveEntryCount - 1
			if mode == "manifest" {
				for i := 0; i < 2; i++ {
					root = filepath.Join(root, strings.Repeat("d", 200))
				}
				os.MkdirAll(root, 0700)
				count = 7000
			}
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%s-%05d", strings.Repeat("f", 200), i)), nil, 0000); err != nil {
					t.Fatal(err)
				}
			}
			// Stop on the first child. Even with thousands of long names, the
			// walker must allocate only one directory batch before yielding.
			stop := errors.New("enough entries")
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err := walkBackupTree(context.Background(), root, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path != root {
					return stop
				}
				return nil
			})
			runtime.ReadMemStats(&after)
			if !errors.Is(err, stop) || after.TotalAlloc-before.TotalAlloc > 512<<10 {
				t.Fatalf("directory scan was not bounded: err=%v allocated=%d", err, after.TotalAlloc-before.TotalAlloc)
			}
			w := httptest.NewRecorder()
			s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil))
			if w.Code != 500 || !strings.Contains(w.Body.String(), map[string]string{"files": "file-count", "manifest": "manifest"}[mode]) {
				t.Fatalf("not rejected before streaming: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

type delayedBackupBody struct {
	in    *bytes.Reader
	first bool
}

func (r *delayedBackupBody) Read(p []byte) (int, error) {
	if !r.first {
		r.first = true
		time.Sleep(60 * time.Millisecond)
	}
	return r.in.Read(p)
}
func TestRestoreOverridesShortServerReadDeadline(t *testing.T) {
	s := newTestServer(t)
	s.dbPath = s.store.path
	w := httptest.NewRecorder()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.restorePlatformSnapshot(&platformBackupAuditWriter{ResponseWriter: w}, r)
	}))
	server.Config.ReadTimeout = 20 * time.Millisecond
	server.Start()
	defer server.Close()
	body := &delayedBackupBody{in: bytes.NewReader(w.Body.Bytes())}
	req, _ := http.NewRequest("POST", server.URL, body)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("long restore hit normal HTTP read deadline: %d", resp.StatusCode)
	}
}

func TestRestoreStageReusesImmutableFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "stage")
	dst := filepath.Join(dir, "prepared")
	os.WriteFile(src, []byte("data"), 0600)
	if err := backupStageFile(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)
	if !os.SameFile(a, b) {
		t.Fatal("restore duplicated data on the same filesystem")
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(dst); err != nil || string(raw) != "data" {
		t.Fatal("cleanup removed staged recovery data")
	}
	if err := backupStageFile(context.Background(), dst, dst); err == nil {
		t.Fatal("restore overwrote an existing staged destination")
	}
}

func TestRestoreRejectsBrokenGzipTrailer(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.writePlatformSnapshot(w, httptest.NewRequest("GET", "/", nil))
	raw := w.Body.Bytes()
	// Tar can be complete while gzip is missing its checksum/trailer.
	broken := raw[:len(raw)-8]
	out := httptest.NewRecorder()
	s.restorePlatformSnapshot(out, httptest.NewRequest("POST", "/", bytes.NewReader(broken)))
	if out.Code != 400 {
		t.Fatalf("accepted corrupt trailer: %d %s", out.Code, out.Body.String())
	}
}

func TestCompressedSnapshotWriterLimit(t *testing.T) {
	var out bytes.Buffer
	limit := &runtimeBoundedWriter{ctx: context.Background(), out: &out, remaining: 1024}
	gz, _ := gzip.NewWriterLevel(limit, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	data := make([]byte, 4096)
	rand.Read(data)
	tw.WriteHeader(&tar.Header{Name: "random", Size: int64(len(data))})
	tw.Write(data)
	err := tw.Close()
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		t.Fatal("compressed ceiling was bypassed")
	}
	if out.Len() > 1024 {
		t.Fatal("writer exceeded ceiling")
	}
}
