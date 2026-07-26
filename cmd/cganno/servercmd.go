package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/compgenlab/cganno/internal/server"
)

// cmdServer runs the async REST annotation server (`cganno server`). It reuses the
// same config + snapshot + annotation-cache store as the CLI, adds a job-queue
// database, and serves HTTP until interrupted (SIGINT/SIGTERM).
func cmdServer(ctx context.Context, cfgPath, snapshot string, args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	addr := fs.String("addr", "", "override the [server] endpoint (IP:port) to listen on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := applyTempDir(cfg.TempDir); err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.Endpoint = *addr
	}
	if cfg.Server.Endpoint == "" {
		return fmt.Errorf("no server endpoint configured — add [server] endpoint = \"IP:port\" to config.toml (or pass -addr)")
	}
	if cfg.Server.RequireTokenForV1() && cfg.Server.MasterKey == "" {
		return fmt.Errorf("no server master_key configured — add [server] master_key = \"...\" to config.toml (or set require_token = false for an open public API)")
	}
	snap, err := cfg.LoadSnapshot(snapshot)
	if err != nil {
		return err
	}

	// Shared annotation-cache store (nil when the cache is disabled).
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if st != nil {
			st.Close()
		}
	}()
	if st != nil {
		if err := st.Init(ctx); err != nil {
			return err
		}
	}

	q, err := server.OpenQueue(ctx, cfg.ServerDBPathAbs())
	if err != nil {
		return err
	}
	defer q.Close()

	// Stop serving on interrupt so in-flight jobs finish and DBs close cleanly.
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, snap, st, q, version)
	return srv.Run(runCtx)
}
