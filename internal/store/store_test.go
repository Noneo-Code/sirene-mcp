package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

// buildDB builds a database from ul (and the default établissements) in its own
// temp dir and returns its path.
func buildDB(t *testing.T, ul []sirenetest.UL) string {
	t.Helper()
	_, etab := sirenetest.Default()
	dir := t.TempDir()
	ulPath, etabPath := sirenetest.Write(t, dir, ul, etab)
	out := filepath.Join(dir, "sirene.duckdb")
	if _, err := syncer.Build(context.Background(), syncer.BuildOptions{
		ULParquet: ulPath, EtabParquet: etabPath, OutPath: out,
		MemoryLimit: "1GB", TempDir: filepath.Join(dir, "tmp"),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return out
}

func dbA(t *testing.T) string {
	t.Helper()
	ul, _ := sirenetest.Default()
	return buildDB(t, ul)
}

func dbB(t *testing.T) string {
	t.Helper()
	ul, _ := sirenetest.Default()
	extra := sirenetest.UL{}
	for k, v := range ul[0] {
		extra[k] = v
	}
	extra["siren"] = "999999999"
	extra["denominationUniteLegale"] = "Sixième"
	return buildDB(t, append(ul, extra))
}

// place copies the built database at src to dst through a temp file + rename,
// like sirene-mcp sync does.
func place(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { //nolint:gosec // G703: test-only temp path
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func countUL(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	err := s.Use(context.Background(), func(db *sql.DB) error {
		return db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n)
	})
	if err != nil {
		t.Fatalf("Use: %v", err)
	}
	return n
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s := Open(path)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUseWithoutDatabase(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "sirene.duckdb"))
	called := false
	err := s.Use(context.Background(), func(*sql.DB) error { called = true; return nil })
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("Use err = %v, want ErrNoDatabase", err)
	}
	err = s.UseLocked(context.Background(), func(*sql.DB) error { called = true; return nil })
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("UseLocked err = %v, want ErrNoDatabase", err)
	}
	if called {
		t.Fatal("fn called without a database")
	}
}

func TestDatabaseRemovedAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	s := openStore(t, path)
	if n := countUL(t, s); n != 5 {
		t.Fatalf("count = %d, want 5", n)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	err := s.Use(context.Background(), func(*sql.DB) error { return nil })
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("Use err = %v, want ErrNoDatabase", err)
	}
	place(t, dbB(t), path)
	if n := countUL(t, s); n != 6 {
		t.Fatalf("count after re-create = %d, want 6", n)
	}
}

func TestReopenAfterSwap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	b := dbB(t)
	s := openStore(t, path)
	if n := countUL(t, s); n != 5 {
		t.Fatalf("count = %d, want 5", n)
	}
	place(t, b, path)
	if n := countUL(t, s); n != 6 {
		t.Fatalf("count after swap = %d, want 6", n)
	}
	var n int64
	if err := s.UseLocked(context.Background(), func(db *sql.DB) error {
		return db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n)
	}); err != nil || n != 6 {
		t.Fatalf("UseLocked after swap = %d, %v; want 6", n, err)
	}
}

func TestInFlightQuerySurvivesSwap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	b := dbB(t)
	s := openStore(t, path)

	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Use(context.Background(), func(db *sql.DB) error {
			close(started)
			<-release
			var n int64
			if err := db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n); err != nil {
				return err
			}
			if n != 5 {
				return errors.New("in-flight query did not see A")
			}
			return nil
		})
	}()
	<-started
	place(t, b, path)

	// The next Use must wait for the in-flight one, then see B.
	next := make(chan int64, 1)
	go func() {
		var n int64
		if err := s.Use(context.Background(), func(db *sql.DB) error {
			return db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n)
		}); err != nil {
			n = -1
		}
		next <- n
	}()
	select {
	case n := <-next:
		t.Fatalf("Use returned %d while a user still held the old pools", n)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight Use: %v", err)
	}
	if n := <-next; n != 6 {
		t.Fatalf("count after swap = %d, want 6", n)
	}
}

func TestTwoStoresReadConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	s1, s2 := openStore(t, path), openStore(t, path)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	barrier := make(chan struct{})
	for _, s := range []*Store{s1, s2} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			errs <- s.Use(context.Background(), func(db *sql.DB) error {
				<-barrier // both stores hold an open handle at the same time
				var n int64
				if err := db.QueryRow("SELECT count(*) FROM etablissement").Scan(&n); err != nil {
					return err
				}
				if n != 6 {
					return errors.New("wrong count")
				}
				return nil
			})
		}(s)
	}
	time.Sleep(100 * time.Millisecond)
	close(barrier)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLockedPoolBlocksFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	other := dbB(t)
	s := openStore(t, path)
	ctx := context.Background()

	// Warm the main pool first: its settings must not leak into the locked pool
	// and vice versa.
	countUL(t, s)
	for _, q := range []string{
		"SELECT * FROM read_csv('/etc/hosts')",
		"SET enable_external_access=true",
		"SET lock_configuration=false",
		"ATTACH '" + other + "' AS other (READ_ONLY)",
		"COPY (SELECT 1) TO '" + other + ".csv'",
		"LOAD httpfs",
	} {
		err := s.UseLocked(ctx, func(db *sql.DB) error {
			_, err := db.ExecContext(ctx, q)
			return err
		})
		if err == nil {
			t.Errorf("UseLocked %q: no error", q)
		}
	}
	// The locked pool still answers normal queries, on every connection.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int64
			err := s.UseLocked(ctx, func(db *sql.DB) error {
				return db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n)
			})
			if err != nil || n != 5 {
				t.Errorf("UseLocked count = %d, %v", n, err)
			}
		}()
	}
	wg.Wait()

	// The main pool keeps external access (export uses COPY … TO) and is read-only.
	out := filepath.Join(t.TempDir(), "out.csv")
	if err := s.Use(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "COPY (SELECT siren FROM unite_legale) TO '"+out+"' (HEADER)") //nolint:gosec // G202: test-only temp path
		return err
	}); err != nil {
		t.Fatalf("main pool COPY TO: %v", err)
	}
	if err := s.Use(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "DELETE FROM unite_legale")
		return err
	}); err == nil {
		t.Fatal("main pool accepted a write")
	}
}

func fileState(t *testing.T, path string) (os.FileInfo, []byte) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi, data
}

// lockedRecovers runs each hostile statement on the locked pool, then checks
// that the locked pool (on fresh and reused connections) and the main pool still
// see the untouched database.
func lockedRecovers(t *testing.T, hostile ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	s := openStore(t, path)
	ctx := context.Background()
	countUL(t, s)
	fi, data := fileState(t, path)

	for _, q := range hostile {
		_ = s.UseLocked(ctx, func(db *sql.DB) error {
			_, err := db.ExecContext(ctx, q)
			return err
		})
		for i := range 3 {
			var n int64
			err := s.UseLocked(ctx, func(db *sql.DB) error {
				return db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n)
			})
			if err != nil || n != 5 {
				t.Fatalf("after %q, UseLocked #%d = %d, %v; want 5", q, i, n, err)
			}
		}
		var leftovers int64
		if err := s.UseLocked(ctx, func(db *sql.DB) error {
			return db.QueryRow("SELECT count(*) FROM duckdb_tables() WHERE database_name <> 'sirene'").Scan(&leftovers)
		}); err != nil || leftovers != 0 {
			t.Fatalf("after %q, tables outside sirene = %d, %v", q, leftovers, err)
		}
		if n := countUL(t, s); n != 5 {
			t.Fatalf("after %q, main pool count = %d", q, n)
		}
	}
	fi2, data2 := fileState(t, path)
	if !sameFile(fi, fi2) || string(data) != string(data2) {
		t.Fatal("database file changed")
	}
}

func TestLockedPoolRecoversFromUse(t *testing.T) {
	lockedRecovers(t, "USE memory", "USE system")
}

func TestLockedPoolRecoversFromDetach(t *testing.T) {
	lockedRecovers(t,
		"USE memory; DETACH sirene",
		"USE memory; DETACH DATABASE IF EXISTS sirene; ATTACH ':memory:' AS sirene",
	)
}

func TestLockedPoolRecoversFromLeftovers(t *testing.T) {
	lockedRecovers(t,
		"CREATE TABLE memory.main.t AS SELECT * FROM range(1000)",
		"ATTACH ':memory:' AS x; CREATE TABLE x.t AS SELECT 1",
		"USE memory; CREATE VIEW unite_legale AS SELECT 1 AS siren",
		"CREATE TEMP TABLE unite_legale AS SELECT 1 AS siren",
	)
}

func TestLockedPoolNotRebuiltWhenHealthy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sirene.duckdb")
	place(t, dbA(t), path)
	s := openStore(t, path)
	ctx := context.Background()
	query := func(db *sql.DB) error {
		var n int64
		return db.QueryRow("SELECT count(*) FROM unite_legale").Scan(&n)
	}
	if err := s.UseLocked(ctx, query); err != nil {
		t.Fatal(err)
	}
	var limit string
	if err := s.UseLocked(ctx, func(db *sql.DB) error {
		return db.QueryRow("SELECT current_setting('memory_limit')").Scan(&limit)
	}); err != nil || limit == "" {
		t.Fatalf("memory_limit = %q, %v", limit, err)
	}
	t.Logf("locked memory_limit = %s", limit)
	first := s.locked
	if !lockedHealthy(first, s.lockedPath) {
		t.Fatal("fresh locked pool reported unhealthy")
	}
	for range 3 {
		if err := s.UseLocked(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if s.locked != first {
		t.Fatal("healthy locked pool was rebuilt")
	}
	_ = s.UseLocked(ctx, func(db *sql.DB) error { _, err := db.Exec("USE memory; DETACH sirene"); return err })
	if s.locked == first {
		t.Fatal("broken locked pool was not rebuilt")
	}
}
