package syncer

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/Noneo-Code/sirene-mcp/internal/config"
	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
)

type fixtureServer struct {
	*httptest.Server
	dir  atomic.Value // string: directory holding the parquet files
	opts Options
}

func (f *fixtureServer) set(t *testing.T, ul []sirenetest.UL, etab []sirenetest.Etab) {
	t.Helper()
	dir := t.TempDir()
	sirenetest.Write(t, dir, ul, etab)
	f.dir.Store(dir)
}

func newFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := "unite_legale.parquet"
		if r.URL.Path == "/etab" {
			name = "etablissement.parquet"
		}
		http.ServeFile(w, r, filepath.Join(fs.dir.Load().(string), name))
	}))
	t.Cleanup(fs.Close)
	ul, etab := sirenetest.Default()
	fs.set(t, ul, etab)
	fs.opts = Options{ULURL: fs.URL + "/ul", EtabURL: fs.URL + "/etab", MemoryLimit: "1GB"}
	return fs
}

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	p, err := config.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func countUL(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("duckdb", path+"?access_mode=read_only")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRunCreatesDatabase(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	stats, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ULRows != 5 || stats.EtabRows != 6 {
		t.Errorf("stats = %+v", stats)
	}
	if !exists(p.DB) || exists(p.NewDB) {
		t.Errorf("DB exists=%v NewDB exists=%v", exists(p.DB), exists(p.NewDB))
	}
	if m, _ := filepath.Glob(filepath.Join(p.WorkDir, "*.parquet")); len(m) != 0 {
		t.Errorf("leftover downloads: %v", m)
	}
}

func TestRunKeepDownloads(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	fs.opts.KeepDownloads = true
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(p.WorkDir, "*.parquet")); len(m) != 2 {
		t.Errorf("downloads = %v", m)
	}
}

func TestRunSecondSyncReplacesFile(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	ul, etab := sirenetest.Default()
	ul = append(ul, sirenetest.UL{"siren": "999999999", "statutDiffusionUniteLegale": "O",
		"etatAdministratifUniteLegale": "A", "denominationUniteLegale": "Nouvelle"})
	fs.set(t, ul, etab)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	if n := countUL(t, p.DB); n != 6 {
		t.Errorf("UL = %d, want 6", n)
	}
}

func TestRunRejectsCollapse(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	ul, etab := sirenetest.Default()
	fs.set(t, ul[:1], etab[:1])
	_, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog())
	if err == nil || !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("err = %v, want ratio error", err)
	}
	if n := countUL(t, p.DB); n != 5 {
		t.Errorf("old DB UL = %d, want 5", n)
	}
	if exists(p.NewDB) {
		t.Error("NewDB not removed")
	}
}

func TestRunCleansStaleNew(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.NewDB, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
}

func TestRunRepairsCorruptDB(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DB, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	if n := countUL(t, p.DB); n != 5 {
		t.Errorf("UL = %d", n)
	}
}

func TestRunLocked(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	l := flock.New(p.LockFile)
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer func() { _ = l.Unlock() }()
	_, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog())
	if err == nil || !strings.Contains(err.Error(), "another sync") {
		t.Fatalf("err = %v", err)
	}
}

func TestNextRun(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, paris) }
	cases := []struct{ now, want time.Time }{
		{at(2026, 11, 4, 10, 0), at(2026, 11, 5, 2, 0)},
		{at(2026, 11, 5, 1, 59), at(2026, 11, 5, 2, 0)},
		{at(2026, 11, 5, 2, 0), at(2026, 12, 5, 2, 0)},
		{at(2026, 12, 20, 0, 0), at(2027, 1, 5, 2, 0)},
	}
	for _, c := range cases {
		if got := NextRun(c.now); !got.Equal(c.want) {
			t.Errorf("NextRun(%v) = %v, want %v", c.now, got, c.want)
		}
	}
	// Works from a non-Paris zone too.
	if got := NextRun(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)); !got.Equal(at(2026, 11, 5, 2, 0)) {
		t.Errorf("UTC input: %v", got)
	}
}
