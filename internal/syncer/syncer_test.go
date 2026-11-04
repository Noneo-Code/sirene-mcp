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
	dir      atomic.Value // string: directory holding the parquet files
	etabMode atomic.Value // string: "" (serve the file), "500" or "garbage"
	opts     Options
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
			switch fs.etabMode.Load() {
			case "500":
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			case "garbage":
				_, _ = w.Write([]byte("this is not a parquet file, just garbage bytes"))
				return
			}
		}
		http.ServeFile(w, r, filepath.Join(fs.dir.Load().(string), name))
	}))
	fs.etabMode.Store("")
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

// assertUntouched checks that a failed sync left the previous database (5 legal
// units) live and cleaned up the new database and every download.
func assertUntouched(t *testing.T, p config.Paths) {
	t.Helper()
	if n := countUL(t, p.DB); n != 5 {
		t.Errorf("live DB UL = %d, want 5", n)
	}
	for _, f := range []string{p.NewDB, p.NewDB + ".wal"} {
		if exists(f) {
			t.Errorf("%s left behind", f)
		}
	}
	for _, pat := range []string{"*.parquet", "*.parquet.part", "*.part"} {
		if m, _ := filepath.Glob(filepath.Join(p.WorkDir, pat)); len(m) != 0 {
			t.Errorf("leftover downloads: %v", m)
		}
	}
}

func fastDownloadRetries(t *testing.T) {
	t.Helper()
	old := downloadBackoff
	downloadBackoff = time.Millisecond
	t.Cleanup(func() { downloadBackoff = old })
}

func TestRunFailedDownloadKeepsLiveDB(t *testing.T) {
	fastDownloadRetries(t)
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	fs.etabMode.Store("500")
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err == nil {
		t.Fatal("want an error when /etab returns 500")
	}
	assertUntouched(t, p)
}

func TestRunGarbageParquetKeepsLiveDB(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	fs.etabMode.Store("garbage")
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err == nil {
		t.Fatal("want an error when /etab serves garbage")
	}
	assertUntouched(t, p)
}

func TestRunRejectsOneSidedCollapse(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	ul, etab := sirenetest.Default()
	fs.set(t, ul, etab[:2]) // only etablissement drops: 6 -> 2
	_, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog())
	if err == nil || !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("err = %v, want ratio error", err)
	}
	assertUntouched(t, p)
}

func TestRunCustomMinRatio(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	ul, etab := sirenetest.Default()
	fs.set(t, ul[:4], etab) // 5 -> 4 legal units: ratio 0.8
	fs.opts.MinRatio = 0.75
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatalf("ratio 0.8 with MinRatio 0.75: %v", err)
	}
	if n := countUL(t, p.DB); n != 4 {
		t.Errorf("UL = %d, want 4", n)
	}
	fs.set(t, ul[:3], etab) // 4 -> 3: ratio 0.75, below 0.8
	fs.opts.MinRatio = 0.8
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err == nil || !strings.Contains(err.Error(), "0.80") {
		t.Fatalf("ratio 0.75 with MinRatio 0.8: err = %v, want rejection", err)
	}
}

func TestRunExcludeClosedModeChangeSkipsRatio(t *testing.T) {
	fs, p := newFixtureServer(t), testPaths(t)
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatal(err)
	}
	ul, etab := sirenetest.Default()
	fs.set(t, ul[:1], etab[:1]) // would fail the ratio check in the same mode
	fs.opts.ExcludeClosed = true
	if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err != nil {
		t.Fatalf("mode change must skip the ratio check: %v", err)
	}
}

func TestRunRejectsEmptyTables(t *testing.T) {
	ul, etab := sirenetest.Default()
	for name, c := range map[string]struct {
		ul   []sirenetest.UL
		etab []sirenetest.Etab
	}{
		"no establishments": {ul, nil},
		"no legal units":    {nil, etab},
	} {
		t.Run(name, func(t *testing.T) {
			// First sync, no previous database: the ratio check cannot apply.
			fs, p := newFixtureServer(t), testPaths(t)
			fs.set(t, c.ul, c.etab)
			_, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog())
			if err == nil || !strings.Contains(err.Error(), "empty table") {
				t.Fatalf("first sync: err = %v, want empty table rejection", err)
			}
			if exists(p.DB) || exists(p.NewDB) {
				t.Error("an empty database was left on disk")
			}
			// Unreadable previous database and a mode change do not excuse it either.
			if err := os.WriteFile(p.DB, []byte("garbage"), 0o600); err != nil {
				t.Fatal(err)
			}
			fs.opts.ExcludeClosed = true
			if _, err := Run(context.Background(), p, fs.Client(), fs.opts, quietLog()); err == nil || !strings.Contains(err.Error(), "empty table") {
				t.Fatalf("over corrupt DB: err = %v, want empty table rejection", err)
			}
		})
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
