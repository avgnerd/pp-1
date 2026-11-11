// Command server runs the Dropbox VFS manager.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // served only on the optional -pprof listener
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/server"
)

func main() {
	dataDir := flag.String("data", envOr("DVFS_DATA", "data"), "data directory (config.json, app.db, caches)")
	pprofAddr := flag.String("pprof", "", "optional pprof listen address, e.g. 127.0.0.1:6060 (keep it on localhost)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load(*dataDir)
	if err != nil {
		fatal("config", err)
	}
	d, err := db.Open(cfg.DataDir)
	if err != nil {
		fatal("database", err)
	}
	defer d.Close()

	srv, err := server.New(cfg, d)
	if err != nil {
		fatal("server", err)
	}

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: video streams and ZIP downloads run for hours.
	}

	if *pprofAddr != "" {
		go func() {
			slog.Info("pprof listening", "addr", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, http.DefaultServeMux); err != nil {
				slog.Error("pprof", "err", err)
			}
		}()
	}

	go func() {
		slog.Info("listening", "addr", cfg.Listen, "data", cfg.DataDir, "public_url", cfg.PublicURL,
			"ffmpeg", cfg.FFmpegPath != "", "ffprobe", cfg.FFprobePath != "")
		fmt.Printf("\n  Dropbox VFS: http://localhost:%s  (첫 로그인: admin / 1234)\n\n", cfg.Port())
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("listen", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
	srv.Close()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(what string, err error) {
	slog.Error(what, "err", err)
	os.Exit(1)
}
