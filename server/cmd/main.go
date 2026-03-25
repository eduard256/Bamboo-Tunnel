package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/eduard256/Bamboo-Tunnel/server/internal/tun"
	"github.com/eduard256/Bamboo-Tunnel/server/internal/tunnel"
	"github.com/eduard256/Bamboo-Tunnel/server/internal/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})))

	log := slog.Default()

	port := os.Getenv("BAMBOO_PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	// init modules in order: tunnel first (registers / handler),
	// then web (sets fallback), then tun (needs tunnel write callback)
	tunnel.Init(mux)
	web.Init()
	tun.Init()

	log.Info("bamboo-server starting", "port", port)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	switch os.Getenv("BAMBOO_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
