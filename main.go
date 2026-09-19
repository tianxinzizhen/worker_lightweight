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
//	--token secret          (shared auth secret; empty disables auth)
//	--tls-cert /path/to/cert.pem   (enable HTTPS)
//	--tls-key  /path/to/key.pem    (enable HTTPS)
//	--audit-log ./logs/audit.log   (structured audit log; empty disables)
//	--rate-limit 60                (max requests/min per IP; 0 = unlimited)
//
// Worker flags:
//
//	--server http://127.0.0.1:8080   (or https://...)
//	--id   worker-1
//	--workers 4
//	--pull-every 1s
//	--token secret      (must match the server token)
//	--insecure          (skip TLS cert verification, for self-signed certs)
//	--blocked-commands   (regex; matching commands are rejected before exec)
//	--allowed-commands   (regex; if set, only matching commands are allowed)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
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
		tlsCert := fs.String("tls-cert", "", "TLS certificate path (enable HTTPS)")
		tlsKey := fs.String("tls-key", "", "TLS private key path (enable HTTPS)")
		auditLog := fs.String("audit-log", "", "structured audit log file path (empty = disabled)")
		rateLimit := fs.Int("rate-limit", 0, "max requests/min per IP (0 = unlimited)")
		_ = fs.Parse(os.Args[2:])
		logger.Init(*logLevel)
		runServer(*addr, *db, *syncEvery, *token, *tlsCert, *tlsKey, *auditLog, *rateLimit)
	case "worker":
		serverAddr := fs.String("server", "http://127.0.0.1:8080", "server address")
		id := fs.String("id", "worker-1", "worker id")
		workers := fs.Int("workers", 4, "executor pool size")
		pullEvery := fs.Duration("pull-every", time.Second, "pull poll interval")
		token := fs.String("token", "", "shared auth secret (must match server)")
		insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
		blockedCmds := fs.String("blocked-commands", "", "regex; matching commands are blocked before exec")
		allowedCmds := fs.String("allowed-commands", "", "regex; if set, only matching commands are allowed")
		_ = fs.Parse(os.Args[2:])
		logger.Init(*logLevel)
		runWorker(*serverAddr, *id, *workers, *pullEvery, *token, *insecure, *blockedCmds, *allowedCmds)
	default:
		usage()
	}
}

// runServer boots the HTTP server and waits for a termination signal.
// On signal it asks the HTTP server to Shutdown (draining in-flight
// requests) with a 10s grace period, then closes the store.
func runServer(addr, dbPath string, syncEvery time.Duration, token, tlsCert, tlsKey, auditLogPath string, rateLimit int) {
	// TLS guard: both or neither must be set, otherwise the user probably
	// forgot one and would silently fall back to plain HTTP — a common
	// misconfiguration that completely defeats the purpose of enabling TLS.
	if (tlsCert != "") != (tlsKey != "") {
		logger.L.Error("TLS requires both --tls-cert and --tls-key; one is missing, aborting",
			"tls_cert_set", tlsCert != "", "tls_key_set", tlsKey != "")
		os.Exit(1)
	}

	// P1-2: warn when the server is reachable from non-loopback networks
	// without TLS — easy to accidentally expose a task scheduler on a LAN.
	if isPublicAddr(addr) {
		if tlsCert == "" || tlsKey == "" {
			logger.L.Warn("SECURITY: binding to non-loopback address without TLS — traffic is exposed to LAN sniffing. Consider adding --tls-cert and --tls-key or restricting via firewall.", "addr", addr)
		}
		if token == "" {
			logger.L.Warn("SECURITY: binding to non-loopback address without auth token — anyone on the network can control the scheduler. Set --token.", "addr", addr)
		}
	}

	// P2-2: create independent audit logger writing to its own file so that
	// operator activity is preserved even if the main log is rotated or lost.
	// The file handle is passed to server.New() so Server.Close() can clean it up.
	var auditLogger *slog.Logger
	var auditFile *os.File
	if auditLogPath != "" {
		var err error
		auditFile, err = os.OpenFile(auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			logger.L.Error("audit log open failed", "path", auditLogPath, "err", err)
			os.Exit(1)
		}
		auditLogger = slog.New(slog.NewTextHandler(auditFile, nil))
		logger.L.Info("audit logging enabled", "path", auditLogPath)
	}

	// Log the resolved db path so operators can tell at a glance which file
	// the server is actually using — the default is ./tasks.db but scripts
	// and the --db flag can redirect it, and a stale tasks.db from an older
	// direct run is easy to mistake for the active one.
	logger.L.Info("server starting", "addr", addr, "db", dbPath, "sync_every", syncEvery,
		"auth", token != "", "tls", tlsCert != "" && tlsKey != "",
		"audit", auditLogger != nil, "rate_limit", rateLimit)
	srv, err := server.New(addr, dbPath, syncEvery, token, tlsCert, tlsKey, auditLogger, auditFile, rateLimit)
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

// isPublicAddr returns true if the listen address is not restricted to
// loopback interfaces (127.0.0.1 or ::1). Empty host defaults to public.
func isPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true // unparseable = assume worst case
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return true // ":8080" binds all interfaces
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

// runWorker boots the worker pool and waits for a termination signal. The
// pool blocks until ctx is cancelled, then drains in-flight jobs before
// returning — so a Ctrl-C results in zero orphaned tasks.
func runWorker(serverAddr, id string, workers int, pullEvery time.Duration, token string, insecure bool, blockedCmds, allowedCmds string) {
	if insecure {
		logger.L.Warn("SECURITY: --insecure enabled — TLS certificate verification is DISABLED. Only use this with self-signed certs on trusted networks.", "server", serverAddr)
	}

	pool, err := worker.NewPool(worker.Config{
		ID:           id,
		ServerAddr:   serverAddr,
		Workers:      workers,
		PullEvery:    pullEvery,
		HTTPTimeout:  10 * time.Second,
		Token:        token,
		Insecure:     insecure,
		BlockedRegex: blockedCmds,
		AllowedRegex: allowedCmds,
	})
	if err != nil {
		logger.L.Error("worker init failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.L.Info("worker starting", "id", id, "workers", workers, "server", serverAddr,
		"insecure", insecure, "blocked_cmds", blockedCmds != "", "allowed_cmds", allowedCmds != "")
	pool.Start(ctx)
	logger.L.Info("worker exited")
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: worker_lightweight {server|worker} [flags]")
	fmt.Fprintln(os.Stderr, "  server --addr 127.0.0.1:8080 --db ./tasks.db --token secret --log-level info")
	fmt.Fprintln(os.Stderr, "  server --addr 0.0.0.0:8443 --db ./tasks.db --token secret --tls-cert cert.pem --tls-key key.pem --audit-log ./logs/audit.log --rate-limit 120")
	fmt.Fprintln(os.Stderr, "  worker --server http://127.0.0.1:8080 --id worker-1 --workers 4 --token secret")
	fmt.Fprintln(os.Stderr, "  worker --server https://server:8443 --id worker-1 --workers 4 --token secret --insecure")
	fmt.Fprintln(os.Stderr, "  worker --blocked-commands 'rm\\s+-rf|mkfs' --allowed-commands '^(bash|python|go)\\s+'")
	os.Exit(2)
}
