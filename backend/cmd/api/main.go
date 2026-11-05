package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/config"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/generation"
	"invoice.local/system/backend/internal/httpapi"
	"invoice.local/system/backend/internal/preview"
	"invoice.local/system/backend/internal/render"
)

func main() {
	configFile := flag.String("config", "", "application configuration JSON path")
	flag.Parse()
	cfg, err := config.Load(*configFile)
	if err != nil {
		slog.Error("invalid configuration; service not started")
		os.Exit(1)
	}
	// Production cannot start with an unavailable persistence/auth adapter.
	if cfg.Environment == "production" {
		slog.Error("production adapters are not wired; service not started")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options := httpapi.Options{WebDir: cfg.WebDir}
	if cfg.DatabaseDSNFile != "" || cfg.KeyringFile != "" {
		if cfg.TLSCertFile == "" {
			slog.Error("business development requires HTTPS")
			os.Exit(1)
		}
		// Open the pool on a short-lived context of its own. It must never
		// shadow the signal context: the queue workers and the HTTP server
		// below run on the signal context, and a cancelled startup context
		// here would silently kill both worker goroutines on their first tick.
		openCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pool, err := account.OpenPool(openCtx, cfg.DatabaseDSNFile, false)
		cancel()
		if err != nil {
			slog.Error("database unavailable")
			os.Exit(1)
		}
		defer pool.Close()
		keys, err := account.LoadKeyring(cfg.KeyringFile)
		if err != nil {
			slog.Error("keyring unavailable")
			os.Exit(1)
		}
		svc, err := account.NewService(pool, keys, cfg.PublicOrigin)
		if err != nil {
			slog.Error("account service unavailable")
			os.Exit(1)
		}
		docs, err := document.NewService(pool, keys, svc)
		if err != nil {
			slog.Error("document service unavailable")
			os.Exit(1)
		}
		// Preview assets are compiled into this binary. A broken bundle disables
		// preview only; it never fabricates a placeholder invoice.
		assets, err := render.Load()
		if err != nil {
			slog.Error("render assets unavailable; previews disabled")
		}
		var previews *preview.Service
		var generations *generation.Service
		if assets != nil {
			previews, err = preview.NewService(pool, svc, assets)
			if err != nil {
				slog.Error("preview service unavailable")
				os.Exit(1)
			}
			generations, err = generation.NewService(pool, svc, assets)
			if err != nil {
				slog.Error("generation service unavailable")
				os.Exit(1)
			}
			go runPreviewWorker(ctx, previews)
			go runFormalWorker(ctx, generations)
		}
		options.Business = httpapi.NewAdminAPI(svc, credit.New(pool, keys), docs, previews, generations, keys, cfg.PublicOrigin)
	}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpapi.NewHandler(options), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("development foundation listening; business readiness remains false", "address", cfg.ListenAddr)
	var serveErr error
	if cfg.TLSCertFile != "" {
		serveErr = server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		serveErr = server.ListenAndServe()
	}
	if err := serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed")
		os.Exit(1)
	}
}

// runPreviewWorker drains the preview queue one request at a time and sweeps
// expired previews. It never touches paid artifacts.
func runPreviewWorker(ctx context.Context, service *preview.Service) {
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for {
		for {
			found, err := service.RunOnce(ctx)
			if err != nil {
				// Shutdown cancels the context mid-claim; that is a normal exit,
				// not a render failure, and must not look like one in the log.
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return
				}
				slog.Error("preview render failed", "error", err)
				break
			}
			if !found {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if err := service.Sweep(ctx); err != nil && ctx.Err() == nil {
				slog.Error("preview sweep failed", "error", err)
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// runFormalWorker drains the formal queue, finalizes deliveries after real
// durability probes, and recovers crashed jobs and expired reservations.
func runFormalWorker(ctx context.Context, service *generation.Service) {
	sweep := time.NewTicker(15 * time.Second)
	defer sweep.Stop()
	for {
		for {
			found, err := service.RunOnce(ctx)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return
				}
				slog.Error("formal render failed", "error", err)
				break
			}
			if !found {
				break
			}
		}
		if err := service.RunFinalize(ctx); err != nil && ctx.Err() == nil {
			slog.Error("delivery finalize failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if err := service.Sweep(ctx); err != nil && ctx.Err() == nil {
				slog.Error("generation sweep failed", "error", err)
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
}
