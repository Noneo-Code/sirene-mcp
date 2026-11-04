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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
	t.Cleanup(func() { s.Close() })
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
		_, err := db.ExecContext(ctx, "COPY (SELECT siren FROM unite_legale) TO '"+out+"' (HEADER)")
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
