package web

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/eduard256/Bamboo-Tunnel/server/internal/tunnel"
)

var log *slog.Logger

func Init() {
	log = slog.Default().With("module", "web")

	webDir := os.Getenv("BAMBOO_WEB_DIR")
	if webDir == "" {
		webDir = "./web"
	}

	fileServer := http.FileServer(http.Dir(webDir))

	tunnel.SetWebHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			// CORS preflight -- respond like a real video call API
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)

		case http.MethodPost:
			// POST without valid token = "meeting not found"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"meeting_not_found","message":"The meeting you're trying to join doesn't exist or has ended."}`))

		default:
			// GET and everything else = serve static files
			fileServer.ServeHTTP(w, r)
		}
	})

	log.Info("web handler registered", "dir", webDir)
}
