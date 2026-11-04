package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Noneo-Code/sirene-mcp/internal/config"
	"github.com/Noneo-Code/sirene-mcp/internal/mcpserver"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

// minTokenLen is the shortest bearer token accepted from $SIRENE_MCP_TOKEN.
const minTokenLen = 16

func resolveToken(p config.Paths) (string, error) {
	if raw, ok := os.LookupEnv("SIRENE_MCP_TOKEN"); ok && raw != "" {
		t := strings.TrimSpace(raw)
		if len(t) < minTokenLen {
			return "", fmt.Errorf("$SIRENE_MCP_TOKEN must be at least %d characters (got %d after trimming whitespace)", minTokenLen, len(t))
		}
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
	exportDirFlag(fs, &exportDir)
	if err := syncFlags(fs, &o); err != nil {
		return err
	}
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
	st := store.OpenWith(paths.DB, store.Options{TempDir: paths.WorkDir, MemoryLimit: o.MemoryLimit})
	defer st.Close()
	srv := mcpserver.New(st, exportDir, version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A signal cancels running tools (queries, exports), so that Shutdown
	// below and st.Close do not wait for them.
	mcpserver.CancelOnDone(ctx, srv)

	if autoSync {
		_, statErr := os.Stat(paths.DB)
		client := &http.Client{Timeout: 2 * time.Hour}
		go autoSyncLoop(ctx, statErr == nil, time.Now, time.After, func(ctx context.Context) error {
			st.SetSyncing(true)
			defer st.SetSyncing(false)
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
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}

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

// autoSyncRetries are the waits before each retry of a failed auto-sync: three
// attempts over about an hour, then the loop waits for the next scheduled run.
var autoSyncRetries = []time.Duration{15 * time.Minute, 45 * time.Minute}

// autoSyncLoop syncs at startup when the database is missing, then sleeps until
// each syncer.NextRun and syncs again until ctx is done. A failed sync is
// logged and retried after each of autoSyncRetries; if every attempt fails the
// loop carries on to the next scheduled run. now and after are injected for
// tests.
func autoSyncLoop(ctx context.Context, dbExists bool, now func() time.Time,
	after func(time.Duration) <-chan time.Time, sync func(context.Context) error, log *slog.Logger) {
	run := func() {
		for attempt := 0; ; attempt++ {
			err := sync(ctx)
			if err == nil || ctx.Err() != nil {
				return
			}
			if attempt >= len(autoSyncRetries) {
				log.Error("auto-sync failed, giving up until the next scheduled run", "attempts", attempt+1, "err", err)
				return
			}
			wait := autoSyncRetries[attempt]
			log.Error("auto-sync failed, retrying", "attempt", attempt+1, "retry_in", wait, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-after(wait):
			}
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
