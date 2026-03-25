package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eduard256/Bamboo-Tunnel/client/internal/tun"
	"github.com/eduard256/Bamboo-Tunnel/client/internal/tunnel"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})))

	log := slog.Default()
	log.Info("bamboo-client starting")

	// init modules: tunnel first (connects to server), then tun (needs tunnel callback)
	tunnel.Init()
	tun.Init()

	log.Info("bamboo-client running")

	// block until signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info("bamboo-client stopping")
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
