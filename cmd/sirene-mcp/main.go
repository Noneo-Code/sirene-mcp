// Command sirene-mcp is a Model Context Protocol server over a local DuckDB
// copy of the INSEE Sirene register.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Noneo-Code/sirene-mcp/internal/config"
	"github.com/Noneo-Code/sirene-mcp/internal/mcpserver"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

// Default Sirene stock files on data.gouv.fr (they redirect to the latest
// version).
const (
	defaultULURL   = "https://www.data.gouv.fr/api/1/datasets/r/350182c9-148a-46e0-8389-76c2ec1374a3"
	defaultEtabURL = "https://www.data.gouv.fr/api/1/datasets/r/a29c1297-1f92-4e2a-8f6b-8c902ce96c5f"
)

// version is set at build time with -ldflags "-X main.version=…" (release
// binaries); for `go install …@vX.Y.Z` it falls back to the module version.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

const usage = `sirene-mcp - MCP server over a local DuckDB copy of the INSEE Sirene register

Usage:
  sirene-mcp sync   [--ul-url URL] [--etab-url URL]  Download the stock files and rebuild the local database
                    [--exclude-closed]               Skip closed establishments and legal units
                    [--memory-limit 4GB]             DuckDB memory limit for the build
  sirene-mcp stdio  [--export-dir DIR]               Run the MCP server over stdio
                    [--memory-limit SIZE]            DuckDB memory limit for queries (default 80% of RAM)
  sirene-mcp serve  [--addr 127.0.0.1:8766]          Run the MCP server over Streamable HTTP (bearer token)
                    [--allow-remote]                 Allow a non-loopback address (put it behind TLS)
                    [--auto-sync]                    Refresh the database monthly (accepts the sync flags)
                    [--export-dir DIR] [--memory-limit SIZE]
  sirene-mcp token                                   Print the bearer token used by serve
  sirene-mcp status                                  Show the database and last sync
  sirene-mcp version

Global flags (put them after the command):
  --data-dir DIR    Data directory (default: $SIRENE_MCP_DATA_DIR or <user cache dir>/sirene-mcp)
  --log-level LVL   debug, info, warn or error

Environment variables (a flag on the command line wins):
  SIRENE_MCP_DATA_DIR        --data-dir
  SIRENE_MCP_UL_URL          --ul-url
  SIRENE_MCP_ETAB_URL        --etab-url
  SIRENE_MCP_EXCLUDE_CLOSED  --exclude-closed (true/false, 1/0)
  SIRENE_MCP_MEMORY_LIMIT    --memory-limit
  SIRENE_MCP_EXPORT_DIR      --export-dir
  SIRENE_MCP_TOKEN           bearer token for serve (at least 16 characters; default: generated and stored in the data dir)

Run "sirene-mcp <command> --help" for details.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sirene-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	cmd := args[0]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("sirene-mcp", version)
		return nil
	case "sync":
		return runSync(args[1:])
	case "stdio":
		return runStdio(args[1:])
	case "serve":
		return runServe(args[1:])
	case "token":
		return runToken(args[1:])
	case "status":
		return runStatus(args[1:])
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// common holds the flags every command accepts.
type common struct {
	dataDir  string
	logLevel string
}

func newFlags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &common{}
	fs.StringVar(&c.dataDir, "data-dir", "", "data directory (default $SIRENE_MCP_DATA_DIR or <user cache dir>/sirene-mcp)")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn or error")
	return fs, c
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean environment variable; unset or empty is false.
func envBool(key string) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("$%s: invalid boolean %q (use true or false)", key, v)
	}
	return b, nil
}

// memoryLimitFlag registers --memory-limit with $SIRENE_MCP_MEMORY_LIMIT as default.
func memoryLimitFlag(fs *flag.FlagSet, dst *string, usage string) {
	fs.StringVar(dst, "memory-limit", os.Getenv("SIRENE_MCP_MEMORY_LIMIT"), usage+" ($SIRENE_MCP_MEMORY_LIMIT)")
}

// exportDirFlag registers --export-dir with $SIRENE_MCP_EXPORT_DIR as default.
func exportDirFlag(fs *flag.FlagSet, dst *string) {
	fs.StringVar(dst, "export-dir", os.Getenv("SIRENE_MCP_EXPORT_DIR"), "directory for export files (default <data dir>/exports; $SIRENE_MCP_EXPORT_DIR)")
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// syncFlags registers the sync options shared by `sync` and `serve --auto-sync`.
// Environment variables provide the defaults; flags win.
func syncFlags(fs *flag.FlagSet, o *syncer.Options) error {
	excludeClosed, err := envBool("SIRENE_MCP_EXCLUDE_CLOSED")
	if err != nil {
		return err
	}
	fs.StringVar(&o.ULURL, "ul-url", envOr("SIRENE_MCP_UL_URL", defaultULURL), "unite legale stock URL ($SIRENE_MCP_UL_URL)")
	fs.StringVar(&o.EtabURL, "etab-url", envOr("SIRENE_MCP_ETAB_URL", defaultEtabURL), "etablissement stock URL ($SIRENE_MCP_ETAB_URL)")
	fs.BoolVar(&o.ExcludeClosed, "exclude-closed", excludeClosed, "skip closed establishments and legal units ($SIRENE_MCP_EXCLUDE_CLOSED)")
	memoryLimitFlag(fs, &o.MemoryLimit, "DuckDB memory limit, e.g. 4GB (sync build default 4GB; serve also applies it to queries)")
	return nil
}

func runSync(args []string) error {
	fs, c := newFlags("sync")
	var o syncer.Options
	if err := syncFlags(fs, &o); err != nil {
		return err
	}
	fs.BoolVar(&o.KeepDownloads, "keep-downloads", false, "keep the downloaded Parquet files")
	if err := parse(fs, args); err != nil {
		return err
	}
	if helpRequested(args) {
		return nil
	}
	log, err := newLogger(c.logLevel)
	if err != nil {
		return err
	}
	paths, err := config.Resolve(c.dataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 2 * time.Hour}
	stats, err := syncer.Run(ctx, paths, client, o, log)
	if err != nil {
		return err
	}
	fmt.Printf("sync complete: %d legal units, %d establishments (%s)\n", stats.ULRows, stats.EtabRows, paths.DB)
	return nil
}

func runStdio(args []string) error {
	fs, c := newFlags("stdio")
	var exportDir, memoryLimit string
	exportDirFlag(fs, &exportDir)
	memoryLimitFlag(fs, &memoryLimit, "DuckDB memory limit for queries, e.g. 4GB (default 80% of RAM)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if helpRequested(args) {
		return nil
	}
	log, err := newLogger(c.logLevel)
	if err != nil {
		return err
	}
	paths, err := config.Resolve(c.dataDir)
	if err != nil {
		return err
	}
	if exportDir == "" {
		exportDir = paths.ExportDir
	}
	st := store.OpenWith(paths.DB, store.Options{TempDir: paths.WorkDir, MemoryLimit: memoryLimit})
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("serving MCP over stdio", "db", paths.DB, "export_dir", exportDir)
	if err := mcpserver.New(st, exportDir, version).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}

func runStatus(args []string) error {
	fs, c := newFlags("status")
	if err := parse(fs, args); err != nil {
		return err
	}
	if helpRequested(args) {
		return nil
	}
	paths, err := config.Resolve(c.dataDir)
	if err != nil {
		return err
	}
	return printStatus(os.Stdout, paths)
}

func printStatus(w io.Writer, p config.Paths) error {
	st, err := os.Stat(p.DB)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(w, `no database yet, run "sirene-mcp sync"`)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "database:  %s\nsize:      %.2f GiB\n", p.DB, float64(st.Size())/(1<<30))
	db, err := sql.Open("duckdb", p.DB+"?access_mode=read_only")
	if err != nil {
		return err
	}
	defer db.Close()
	var (
		started, finished            time.Time
		ul, etab                     int64
		closed                       bool
		ulSrc, etabSrc, ulLM, etabLM sql.NullString
	)
	err = db.QueryRow(`SELECT started_at, finished_at, ul_rows, etab_rows, exclude_closed,
		ul_source, etab_source, ul_last_modified, etab_last_modified
		FROM sync_log ORDER BY finished_at DESC LIMIT 1`).
		Scan(&started, &finished, &ul, &etab, &closed, &ulSrc, &etabSrc, &ulLM, &etabLM)
	if err != nil {
		return fmt.Errorf("read last sync: %w", err)
	}
	fmt.Fprintf(w, "last sync: %s (took %s)\n", finished.UTC().Format(time.RFC3339), finished.Sub(started).Round(time.Second))
	fmt.Fprintf(w, "rows:      %d legal units, %d establishments\n", ul, etab)
	fmt.Fprintf(w, "exclude closed: %t\n", closed)
	fmt.Fprintf(w, "unite legale:  %s (last modified %s)\n", ulSrc.String, ulLM.String)
	fmt.Fprintf(w, "etablissement: %s (last modified %s)\n", etabSrc.String, etabLM.String)
	return nil
}
