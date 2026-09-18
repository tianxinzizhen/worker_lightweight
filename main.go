// Command worker_lightweight runs as either a scheduler server or a worker
// node, selected by the first CLI argument. Both modes install a SIGINT /
// SIGTERM handler that triggers graceful shutdown: in-flight requests finish
// (server) and the worker pool drains its jobs channel before exit (worker).
//
// Usage:
//
//	worker_lightweight server  [flags]   # run the API + cron scheduler
//	worker_lightweight worker  [flags]   # run the worker pool
//
// Common flags:
//
//	--log-level debug|info   (default info)
//
// Server flags:
//
//	--addr 127.0.0.1:8080
//	--db   ./tasks.db
//	--token secret   (shared auth secret; empty disables auth)
//
// Worker flags:
//
//	--server http://127.0.0.1:8080
//	--id   worker-1
//	--workers 4
//	--pull-every 1s
//	--token secret   (must match the server token)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tianxinzizhen/worker_lightweight/internal/logger"
	"github.com/tianxinzizhen/worker_lightweight/internal/server"
	"github.com/tianxinzizhen/worker_lightweight/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	logLevel := fs.String("log-level", "info", "log level: debug|info")

	switch mode {
	case "server":
		addr := fs.String("addr", "127.0.0.1:8080", "listen address")
		db := fs.String("db", "./tasks.db", "sqlite path")
		syncEvery := fs.Duration("sync-every", 30*time.Second, "cron resync interval")
		token := fs.String("token", "", "shared auth secret (empty = no auth, local-only)")
		_ = fs.Parse(os.Args[2:])
		logger.Init(*logLevel)
		runServer(*addr, *db, *syncEvery, *token)
	case "worker":
		serverAddr := fs.String("server", "http://127.0.0.1:8080", "server address")
		id := fs.String("id", "worker-1", "worker id")
		workers := fs.Int("workers", 4, "executor pool size")
		pullEvery := fs.Duration("pull-every", time.Second, "pull poll interval")
		token := fs.String("token", "", "shared auth secret (must match server)")
		_ = fs.Parse(os.Args[2:])
		logger.Init(*logLevel)
		runWorker(*serverAddr, *id, *workers, *pullEvery, *token)
	default:
		usage()
	}
}

// runServer boots the HTTP server and waits for a termination signal.
// On signal it asks the HTTP server to Shutdown (draining in-flight
// requests) with a 10s grace period, then closes the store.
func runServer(addr, dbPath string, syncEvery time.Duration, token string) {
	// Log the resolved db path so operators can tell at a glance which file
	// the server is actually using — the default is ./tasks.db but scripts
	// and the --db flag can redirect it, and a stale tasks.db from an older
	// direct run is easy to mistake for the active one.
	logger.L.Info("server starting", "addr", addr, "db", dbPath, "sync_every", syncEvery, "auth", token != "")
	srv, err := server.New(addr, dbPath, syncEvery, token)
	if err != nil {
		logger.L.Error("server init failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Run ListenAndServe in the background; cancel ctx on signal.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case err := <-errCh:
		if err != nil {
			logger.L.Error("server exited with error", "err", err)
		}
	case <-ctx.Done():
		logger.L.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.L.Error("graceful shutdown failed", "err", err)
		}
	}
	if err := srv.Close(); err != nil {
		logger.L.Error("store close failed", "err", err)
	}
	logger.L.Info("server exited")
}

// runWorker boots the worker pool and waits for a termination signal. The
// pool blocks until ctx is cancelled, then drains in-flight jobs before
// returning — so a Ctrl-C results in zero orphaned tasks.
func runWorker(serverAddr, id string, workers int, pullEvery time.Duration, token string) {
	pool := worker.NewPool(worker.Config{
		ID:          id,
		ServerAddr:  serverAddr,
		Workers:     workers,
		PullEvery:   pullEvery,
		HTTPTimeout: 10 * time.Second,
		Token:       token,
	})

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.L.Info("worker starting", "id", id, "workers", workers, "server", serverAddr)
	pool.Start(ctx)
	logger.L.Info("worker exited")
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: worker_lightweight {server|worker} [flags]")
	fmt.Fprintln(os.Stderr, "  server --addr 127.0.0.1:8080 --db ./tasks.db --token secret --log-level info")
	fmt.Fprintln(os.Stderr, "  worker --server http://127.0.0.1:8080 --id worker-1 --workers 4 --token secret")
	os.Exit(2)
}
