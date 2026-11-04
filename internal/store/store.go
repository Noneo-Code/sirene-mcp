// Package store gives read-only access to the Sirene DuckDB database and
// follows atomic swaps of the file by sirene-mcp sync.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// ErrNoDatabase is returned while the database file does not exist.
var ErrNoDatabase = errors.New(`no Sirene database yet: run "sirene-mcp sync"`)

// Store is a lazily opened, read-only handle on the database file. It is safe
// for concurrent use.
type Store struct {
	path string

	mu     sync.RWMutex
	info   os.FileInfo // identity of the file the pools were opened on
	main   *sql.DB
	locked *sql.DB
	closed bool
}

// Open returns a Store for path. It never fails: the file is opened on first use.
func Open(path string) *Store { return &Store{path: path} }

// Use runs fn with the main read-only pool.
func (s *Store) Use(ctx context.Context, fn func(db *sql.DB) error) error {
	return s.use(ctx, false, fn)
}

// UseLocked runs fn with the locked-down pool meant for user SQL: no file or
// network access, configuration locked.
func (s *Store) UseLocked(ctx context.Context, fn func(db *sql.DB) error) error {
	return s.use(ctx, true, fn)
}

func (s *Store) use(ctx context.Context, locked bool, fn func(db *sql.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		fi, err := s.stat()
		if errors.Is(err, ErrNoDatabase) {
			s.dropPools() // release the handle on the deleted file
		}
		if err != nil {
			return err
		}
		s.mu.RLock()
		if s.closed {
			s.mu.RUnlock()
			return errors.New("store: closed")
		}
		if s.info != nil && sameFile(s.info, fi) {
			db := s.main
			if locked {
				db = s.locked
			}
			defer s.mu.RUnlock()
			return fn(db)
		}
		s.mu.RUnlock()
		if err := s.reopen(fi); err != nil {
			return err
		}
	}
}

func (s *Store) stat() (os.FileInfo, error) {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDatabase
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return fi, nil
}

func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// reopen swaps the pools for ones opened on the file described by fi, unless
// another caller already did. It waits for in-flight users of the old pools.
func (s *Store) reopen(fi os.FileInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("store: closed")
	}
	if s.info != nil && sameFile(s.info, fi) {
		return nil
	}
	s.closePools()
	main, locked, err := openPools(s.path)
	if err != nil {
		return err
	}
	s.main, s.locked, s.info = main, locked, fi
	return nil
}

// openPools opens the main and locked pools on path.
//
// Each pool gets its own in-memory DuckDB instance that ATTACHes the file
// READ_ONLY, rather than opening "path?access_mode=read_only" directly: the
// driver shares one instance per file path (its instance cache), so the locked
// pool's global settings (enable_external_access, lock_configuration) would
// leak into the main pool, and a reopen while another handle on the same path
// is alive would get the old file back.
func openPools(path string) (main, locked *sql.DB, err error) {
	main, err = openPool(path, nil)
	if err != nil {
		return nil, nil, err
	}
	locked, err = openPool(path, []string{
		"SET enable_external_access=false",
		"SET lock_configuration=true",
	})
	if err != nil {
		main.Close()
		return nil, nil, err
	}
	return main, locked, nil
}

func openPool(path string, settings []string) (*sql.DB, error) {
	var ready atomic.Bool
	c, err := duckdb.NewConnector(":memory:", func(execer driver.ExecerContext) error {
		if !ready.Load() {
			return nil // the setup connection below
		}
		_, err := execer.ExecContext(context.Background(), "USE sirene", nil)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := setup(c, path, settings); err != nil {
		c.Close()
		return nil, err
	}
	ready.Store(true)
	db := sql.OpenDB(c)
	db.SetConnMaxIdleTime(time.Minute)
	return db, nil
}

// setup attaches the file and applies instance-wide settings, on a connection
// of its own before the pool hands any out.
func setup(c *duckdb.Connector, path string, settings []string) error {
	ctx := context.Background()
	conn, err := c.Connect(ctx)
	if err != nil {
		return fmt.Errorf("store: open %s: %w", path, err)
	}
	defer conn.Close()
	execer := conn.(driver.ExecerContext)
	stmts := append([]string{
		"ATTACH '" + strings.ReplaceAll(path, "'", "''") + "' AS sirene (READ_ONLY)",
	}, settings...)
	for _, q := range stmts {
		if _, err := execer.ExecContext(ctx, q, nil); err != nil {
			if errors.Is(err, os.ErrNotExist) || !fileExists(path) {
				return ErrNoDatabase
			}
			return fmt.Errorf("store: open %s: %w", path, err)
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dropPools closes the pools, if any, once in-flight users are done.
func (s *Store) dropPools() {
	s.mu.RLock()
	open := s.info != nil
	s.mu.RUnlock()
	if !open {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		s.closePools()
	}
}

func (s *Store) closePools() {
	if s.main != nil {
		s.main.Close()
	}
	if s.locked != nil {
		s.locked.Close()
	}
	s.main, s.locked, s.info = nil, nil, nil
}

// Close closes the pools. Later calls to Use fail.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closePools()
	s.closed = true
	return nil
}
