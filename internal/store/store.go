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

// ErrSyncInProgress replaces ErrNoDatabase while the first sync is building
// the database (see SetSyncing).
var ErrSyncInProgress = errors.New("the Sirene database is being built by a sync in progress (download and build take a while); retry later")

// Options tunes the main read-only pool.
type Options struct {
	// TempDir is where DuckDB spills large queries; "" leaves DuckDB's default
	// (a .tmp directory in the process's working directory).
	TempDir string
	// MemoryLimit caps the main pool, in DuckDB syntax ("4GB"); "" leaves
	// DuckDB's default (80% of RAM).
	MemoryLimit string
}

// Store is a lazily opened, read-only handle on the database file. It is safe
// for concurrent use.
//
// The pools opened on one version of the file form a generation. Each Use
// takes a reference on the current generation's pool and drops it when fn
// returns; a swap of the file publishes a new generation without waiting and
// closes the old pools once their last user is done. The mutex is only held to
// take or drop references and to publish a generation, never while fn runs or
// while pools open, so no caller waits on another's query.
type Store struct {
	path    string
	opts    Options
	syncing atomic.Bool

	mu      sync.Mutex
	cur     *generation   // nil until first use, after a removal or a failed rebuild
	opening chan struct{} // non-nil while a caller opens a generation; closed when done
	closed  bool

	// beforeOpen, if set, runs before a generation opens (tests).
	beforeOpen func()
}

// generation is the pair of pools opened on one version of the file.
type generation struct {
	info   os.FileInfo // identity of the file the pools were opened on
	main   *pool
	locked *pool // replaced in place by rebuildLocked
}

// pool is a reference-counted *sql.DB. refs and retired are guarded by
// Store.mu; the DB is closed when it is retired and unused.
type pool struct {
	db *sql.DB
	// path is, for a locked pool, the path DuckDB reports for the attached
	// file; the health check compares against it.
	path    string
	refs    int
	retired bool
}

// Open returns a Store for path with default Options. It never fails: the
// file is opened on first use.
func Open(path string) *Store { return OpenWith(path, Options{}) }

// OpenWith is Open with options for the main pool.
func OpenWith(path string, o Options) *Store { return &Store{path: path, opts: o} }

// SetSyncing tells the store whether a sync is running, so that a missing
// database is reported as ErrSyncInProgress rather than ErrNoDatabase.
func (s *Store) SetSyncing(v bool) { s.syncing.Store(v) }

// Use runs fn with the main read-only pool.
//
// fn keeps the pool it was given even if the file is swapped meanwhile: the
// old pools stay open until their last user returns, while later calls get
// pools on the new file. The only wait, for another caller opening the pools,
// ends early with ctx.Err() if ctx is done.
func (s *Store) Use(ctx context.Context, fn func(db *sql.DB) error) error {
	return s.use(ctx, false, fn)
}

// UseLocked runs fn with the locked-down pool meant for user SQL: no file or
// network access, configuration locked. Whatever fn runs (USE, DETACH, ATTACH
// ':memory:', CREATE TABLE in the memory catalog…), the next call gets a clean
// pool: connections are never reused, and after fn a health check rebuilds the
// locked instance if it was altered. Calls running concurrently with a hostile
// fn may still see its effects.
func (s *Store) UseLocked(ctx context.Context, fn func(db *sql.DB) error) error {
	return s.use(ctx, true, fn)
}

func (s *Store) use(ctx context.Context, locked bool, fn func(db *sql.DB) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fi, err := s.stat()
		if errors.Is(err, ErrNoDatabase) {
			s.dropPools() // release the handle on the deleted file
			if s.syncing.Load() {
				return ErrSyncInProgress
			}
		}
		if err != nil {
			return err
		}
		g, p, wait, err := s.acquire(fi, locked)
		if err != nil {
			return err
		}
		if p != nil {
			err := fn(p.db)
			healthy := !locked || lockedHealthy(p.db, p.path)
			s.release(p)
			if !healthy {
				s.rebuildLocked(g, p)
			}
			return err
		}
		if wait != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wait:
			}
			continue
		}
		if err := s.reopen(ctx, fi); err != nil {
			return err
		}
	}
}

// acquire takes a reference on the current generation's main or locked pool
// if it was opened on the file described by fi. Otherwise it returns the
// channel of an opening in progress to wait on, or, with all three results
// nil, makes the caller the opener: it must call reopen.
func (s *Store) acquire(fi os.FileInfo, locked bool) (*generation, *pool, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, nil, errors.New("store: closed")
	}
	if g := s.cur; g != nil && sameFile(g.info, fi) {
		p := g.main
		if locked {
			p = g.locked
		}
		p.refs++
		return g, p, nil, nil
	}
	if s.opening != nil {
		return nil, nil, s.opening, nil
	}
	s.opening = make(chan struct{})
	return nil, nil, nil, nil
}

// release drops a reference taken by acquire.
func (s *Store) release(p *pool) {
	s.mu.Lock()
	p.refs--
	closeNow := p.retired && p.refs == 0
	s.mu.Unlock()
	if closeNow {
		_ = p.db.Close()
	}
}

// retire marks p for closing and returns whether it is unused, in which case
// the caller closes it once s.mu is released. Called with s.mu held.
func retire(p *pool) bool {
	if p == nil || p.retired {
		return false
	}
	p.retired = true
	return p.refs == 0
}

// retireGen retires both pools of g and returns those to close now. Called
// with s.mu held.
func retireGen(g *generation) []*pool {
	if g == nil {
		return nil
	}
	var out []*pool
	for _, p := range []*pool{g.main, g.locked} {
		if retire(p) {
			out = append(out, p)
		}
	}
	return out
}

func closePools(ps []*pool) {
	for _, p := range ps {
		_ = p.db.Close()
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

// reopen opens a generation on the file described by fi and publishes it,
// retiring the previous one without waiting for its users. Only the caller
// that acquire made the opener calls it; it ends the opening either way.
func (s *Store) reopen(ctx context.Context, fi os.FileInfo) error {
	if s.beforeOpen != nil {
		s.beforeOpen()
	}
	g, err := s.openGeneration(ctx, fi)
	s.mu.Lock()
	close(s.opening)
	s.opening = nil
	var toClose []*pool
	switch {
	case err != nil:
	case s.closed:
		toClose = []*pool{g.main, g.locked}
		err = errors.New("store: closed")
	default:
		toClose = retireGen(s.cur)
		s.cur = g
	}
	s.mu.Unlock()
	closePools(toClose)
	return err
}

func (s *Store) openGeneration(ctx context.Context, fi os.FileInfo) (*generation, error) {
	main, err := openPool(ctx, s.path, s.mainSettings())
	if err != nil {
		return nil, err
	}
	locked, err := openLocked(ctx, s.path)
	if err != nil {
		_ = main.Close()
		return nil, err
	}
	return &generation{info: fi, main: &pool{db: main}, locked: locked}, nil
}

// mainSettings are applied once per main instance.
func (s *Store) mainSettings() []string {
	var out []string
	if s.opts.TempDir != "" {
		out = append(out, "SET temp_directory = "+quote(s.opts.TempDir))
	}
	if s.opts.MemoryLimit != "" {
		out = append(out, "SET memory_limit = "+quote(s.opts.MemoryLimit))
	}
	return out
}

// lockedSettings are applied once per locked instance, before the pool is used.
var lockedSettings = []string{
	"SET memory_limit='2GB'",
	"SET enable_external_access=false",
	"SET lock_configuration=true",
}

// openLocked opens the locked pool and records the attached file's path as
// DuckDB reports it.
func openLocked(ctx context.Context, path string) (*pool, error) {
	db, err := openPool(ctx, path, lockedSettings)
	if err != nil {
		return nil, err
	}
	// No idle connections: per-connection state (USE, temp tables) dies with
	// the connection, and every connection starts with USE sirene.
	db.SetMaxIdleConns(0)
	var attached string
	if err := db.QueryRowContext(ctx, "SELECT path FROM duckdb_databases() WHERE database_name = 'sirene'").Scan(&attached); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &pool{db: db, path: attached}, nil
}

// lockedHealthy reports whether the locked instance is still exactly as opened:
// the file attached read-only as sirene, nothing else attached, and nothing
// created in the memory catalog. A fresh connection runs the check, so a
// detached sirene fails its USE init.
func lockedHealthy(db *sql.DB, attached string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ok bool
	err := db.QueryRowContext(ctx, `SELECT
	(SELECT count(*) FROM duckdb_databases() WHERE database_name = 'sirene' AND readonly AND path = `+quote(attached)+`) = 1
	AND (SELECT count(*) FROM duckdb_databases() WHERE NOT internal AND database_name NOT IN ('sirene', 'memory')) = 0
	AND (SELECT count(*) FROM duckdb_tables() WHERE database_name <> 'sirene') = 0
	AND (SELECT count(*) FROM duckdb_views() WHERE NOT internal AND database_name <> 'sirene') = 0`).Scan(&ok)
	return err == nil && ok
}

// rebuildLocked replaces g's locked pool bad with a fresh one, unless another
// caller already did or g is no longer current. The main pool is not touched,
// and users still holding bad keep it until they return.
func (s *Store) rebuildLocked(g *generation, bad *pool) {
	fresh, err := openLocked(context.Background(), s.path)
	s.mu.Lock()
	var toClose []*pool
	switch {
	case s.closed || s.cur != g || g.locked != bad:
		if fresh != nil {
			toClose = []*pool{fresh}
		}
	case err != nil:
		// The next call reopens everything or reports ErrNoDatabase.
		toClose = retireGen(g)
		s.cur = nil
	default:
		// If the file was swapped meanwhile, the next call sees the identity
		// change and opens a new generation.
		if retire(bad) {
			toClose = []*pool{bad}
		}
		g.locked = fresh
	}
	s.mu.Unlock()
	closePools(toClose)
}

func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// openPool opens a pool on path with its own in-memory DuckDB instance that
// ATTACHes the file READ_ONLY, rather than opening "path?access_mode=read_only"
// directly: the driver shares one instance per file path (its instance cache),
// so the locked pool's global settings (enable_external_access,
// lock_configuration) would leak into the main pool, and a reopen while another
// handle on the same path is alive would get the old file back.
func openPool(ctx context.Context, path string, settings []string) (*sql.DB, error) {
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
	if err := setup(ctx, c, path, settings); err != nil {
		_ = c.Close()
		return nil, err
	}
	ready.Store(true)
	db := sql.OpenDB(c)
	db.SetConnMaxIdleTime(time.Minute)
	return db, nil
}

// setup attaches the file and applies instance-wide settings, on a connection
// of its own before the pool hands any out.
func setup(ctx context.Context, c *duckdb.Connector, path string, settings []string) error {
	conn, err := c.Connect(ctx)
	if err != nil {
		return fmt.Errorf("store: open %s: %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	execer := conn.(driver.ExecerContext)
	stmts := append([]string{
		"ATTACH " + quote(path) + " AS sirene (READ_ONLY)",
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

// dropPools retires the current generation, if the file is gone. Its pools
// close once their in-flight users are done.
func (s *Store) dropPools() {
	s.mu.Lock()
	var toClose []*pool
	if s.cur != nil {
		if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
			toClose = retireGen(s.cur)
			s.cur = nil
		}
	}
	s.mu.Unlock()
	closePools(toClose)
}

// Close retires the pools without waiting: those in use close when their last
// user returns. Later calls to Use fail.
func (s *Store) Close() error {
	s.mu.Lock()
	toClose := retireGen(s.cur)
	s.cur = nil
	s.closed = true
	s.mu.Unlock()
	closePools(toClose)
	return nil
}
