package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
	_ "time/tzdata" // NextRun needs Europe/Paris even on minimal servers

	"github.com/gofrs/flock"

	"github.com/Noneo-Code/sirene-mcp/internal/config"
)

// Options configures Run.
type Options struct {
	ULURL, EtabURL string
	ExcludeClosed  bool
	MemoryLimit    string
	KeepDownloads  bool
	MinRatio       float64 // minimum new/previous row ratio; 0 means 0.9
}

const defaultMinRatio = 0.9

// Run downloads the stocks, builds a new database next to the live one,
// sanity-checks it against the previous sync, and atomically swaps it in.
// The live database is never modified before the new one is validated.
func Run(ctx context.Context, p config.Paths, client *http.Client, o Options, log *slog.Logger) (stats BuildStats, err error) {
	if err := p.Ensure(); err != nil {
		return stats, err
	}
	lock := flock.New(p.LockFile)
	ok, err := lock.TryLock()
	if err != nil {
		return stats, fmt.Errorf("lock %s: %w", p.LockFile, err)
	}
	if !ok {
		return stats, errors.New("another sync is already running")
	}
	defer func() { _ = lock.Unlock() }()

	if err := os.MkdirAll(p.WorkDir, 0o700); err != nil {
		return stats, fmt.Errorf("create work directory: %w", err)
	}
	removeNew := func() {
		_ = os.Remove(p.NewDB)
		_ = os.Remove(p.NewDB + ".wal")
	}
	removeNew() // stale leftover from an interrupted run
	ulPath := filepath.Join(p.WorkDir, "unite_legale.parquet")
	etabPath := filepath.Join(p.WorkDir, "etablissement.parquet")
	defer func() {
		if !o.KeepDownloads {
			for _, f := range []string{ulPath, etabPath, ulPath + ".part", etabPath + ".part"} {
				_ = os.Remove(f)
			}
		}
	}()
	defer func() {
		if err != nil {
			removeNew()
		}
	}()

	total := time.Now()
	step := func(name, url, dest string) (Downloaded, error) {
		start := time.Now()
		log.Info("downloading "+name, "url", url)
		d, err := Download(ctx, client, url, dest)
		if err != nil {
			return d, err
		}
		log.Info("downloaded "+name, "bytes", d.Bytes, "duration", time.Since(start).Round(time.Millisecond))
		return d, nil
	}
	ul, err := step("unite legale stock", o.ULURL, ulPath)
	if err != nil {
		return stats, err
	}
	etab, err := step("etablissement stock", o.EtabURL, etabPath)
	if err != nil {
		return stats, err
	}

	start := time.Now()
	log.Info("building database", "path", p.NewDB, "exclude_closed", o.ExcludeClosed)
	stats, err = Build(ctx, BuildOptions{
		ULParquet: ulPath, EtabParquet: etabPath, OutPath: p.NewDB,
		ExcludeClosed: o.ExcludeClosed, MemoryLimit: o.MemoryLimit, TempDir: p.WorkDir,
		Sources: SourceInfo{ULURL: ul.FinalURL, EtabURL: etab.FinalURL,
			ULLastModified: ul.LastModified, EtabLastModified: etab.LastModified},
	})
	if err != nil {
		return stats, err
	}
	log.Info("built database", "ul_rows", stats.ULRows, "etab_rows", stats.EtabRows,
		"duration", time.Since(start).Round(time.Millisecond))

	if err := checkRatio(p.DB, stats, o, log); err != nil {
		return stats, err
	}
	if err := os.Rename(p.NewDB, p.DB); err != nil {
		return stats, fmt.Errorf("swap database: %w", err)
	}
	_ = os.Remove(p.DB + ".wal") // belongs to the replaced file
	log.Info("sync complete", "db", p.DB, "duration", time.Since(total).Round(time.Millisecond))
	return stats, nil
}

// checkRatio compares the new counts with the live database's last sync_log
// row. An unreadable or missing previous database is not an obstacle: the new
// one may be the repair.
func checkRatio(dbPath string, got BuildStats, o Options, log *slog.Logger) error {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	prev, prevClosed, err := lastSync(dbPath)
	if err != nil {
		log.Warn("cannot read previous database, skipping row ratio check", "error", err)
		return nil
	}
	if prevClosed != o.ExcludeClosed {
		return nil
	}
	min := o.MinRatio
	if min == 0 {
		min = defaultMinRatio
	}
	if float64(got.ULRows) < min*float64(prev.ULRows) || float64(got.EtabRows) < min*float64(prev.EtabRows) {
		return fmt.Errorf("new database rejected: row ratio below %.2f (unite_legale %d vs %d, etablissement %d vs %d); live database left untouched",
			min, got.ULRows, prev.ULRows, got.EtabRows, prev.EtabRows)
	}
	return nil
}

func lastSync(dbPath string) (prev BuildStats, excludeClosed bool, err error) {
	db, err := sql.Open("duckdb", dbPath+"?access_mode=read_only")
	if err != nil {
		return prev, false, err
	}
	defer db.Close() // closed before the caller renames over the file
	err = db.QueryRow(`SELECT ul_rows, etab_rows, exclude_closed FROM sync_log
		ORDER BY finished_at DESC LIMIT 1`).Scan(&prev.ULRows, &prev.EtabRows, &excludeClosed)
	return prev, excludeClosed, err
}

// NextRun returns the next 5th of the month at 02:00 Europe/Paris that is
// strictly after now.
func NextRun(now time.Time) time.Time {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		paris = time.UTC
	}
	n := now.In(paris)
	t := time.Date(n.Year(), n.Month(), 5, 2, 0, 0, 0, paris)
	if !t.After(n) {
		t = time.Date(n.Year(), n.Month()+1, 5, 2, 0, 0, 0, paris)
	}
	return t
}
