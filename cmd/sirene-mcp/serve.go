package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Noneo-Code/sirene-mcp/internal/config"
	"github.com/Noneo-Code/sirene-mcp/internal/mcpserver"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

func resolveToken(p config.Paths) (string, error) {
	if t := os.Getenv("SIRENE_MCP_TOKEN"); t != "" {
		return t, nil
	}
	return mcpserver.LoadOrCreateToken(p.TokenFile)
}

func runToken(args []string) error {
	fs, c := newFlags("token")
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
	t, err := resolveToken(paths)
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}

func runServe(args []string) error {
	fs, c := newFlags("serve")
	var (
		addr, exportDir string
		allowRemote     bool
		autoSync        bool
		o               syncer.Options
	)
	fs.StringVar(&addr, "addr", "127.0.0.1:8766", "listen address")
	fs.BoolVar(&allowRemote, "allow-remote", false, "allow a non-loopback address (put it behind a TLS reverse proxy)")
	fs.BoolVar(&autoSync, "auto-sync", false, "refresh the database on the monthly schedule")
	fs.StringVar(&exportDir, "export-dir", "", "directory for export files (default <data dir>/exports)")
	syncFlags(fs, &o)
	if err := parse(fs, args); err != nil {
		return err
	}
	if helpRequested(args) {
		return nil
	}
	if !allowRemote && !mcpserver.IsLoopback(addr) {
		return fmt.Errorf("refusing to listen on %s: it is reachable from other machines. Use 127.0.0.1, or pass --allow-remote behind TLS", addr)
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
	token, err := resolveToken(paths)
	if err != nil {
		return err
	}
	st := store.Open(paths.DB)
	defer st.Close()
	srv := mcpserver.New(st, exportDir, version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if autoSync {
		_, statErr := os.Stat(paths.DB)
		client := &http.Client{Timeout: 2 * time.Hour}
		go autoSyncLoop(ctx, statErr == nil, time.Now, time.After, func(ctx context.Context) error {
			_, err := syncer.Run(ctx, paths, client, o, log)
			return err
		}, log)
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute,
		// A reverse proxy legitimately sends its own Host header.
		DisableLocalhostProtection: allowRemote,
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.RequireBearer(token, handler))
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("serving MCP over HTTP", "url", "http://"+addr+"/mcp", "auto_sync", autoSync, "db", paths.DB)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// autoSyncLoop syncs at startup when the database is missing, then sleeps until
// each syncer.NextRun and syncs again until ctx is done. A failed sync is
// logged and the loop carries on. now and after are injected for tests.
func autoSyncLoop(ctx context.Context, dbExists bool, now func() time.Time,
	after func(time.Duration) <-chan time.Time, sync func(context.Context) error, log *slog.Logger) {
	run := func() {
		if err := sync(ctx); err != nil && ctx.Err() == nil {
			log.Error("auto-sync failed", "err", err)
		}
	}
	if !dbExists {
		log.Info("no database, syncing at startup")
		run()
	}
	for ctx.Err() == nil {
		t := syncer.NextRun(now())
		log.Info("next auto-sync", "at", t)
		select {
		case <-ctx.Done():
			return
		case <-after(t.Sub(now())):
		}
		run()
	}
}
