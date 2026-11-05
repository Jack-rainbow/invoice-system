package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

type Options struct {
	Ready    func(context.Context) bool
	Business http.Handler
	WebDir   string
}

// NewHandler exposes only infrastructure endpoints until real services are wired.
// A live process is never treated as a ready business application.
func NewHandler(options Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			write(w, 503, map[string]string{"code": "DEPENDENCY_UNAVAILABLE"})
			return
		}
		requestID := hex.EncodeToString(id[:])
		w.Header().Set("X-Request-ID", requestID)
		defer func() {
			if recover() != nil {
				write(w, 503, map[string]string{"code": "DEPENDENCY_UNAVAILABLE", "request_id": requestID})
			}
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				w.Header().Set("Allow", "GET, HEAD")
				write(w, 405, map[string]string{"code": "METHOD_NOT_ALLOWED", "request_id": requestID})
				return
			}
		}
		switch r.URL.Path {
		case "/healthz":
			write(w, 200, map[string]string{"status": "ok"})
		case "/readyz":
			if options.Ready != nil && options.Ready(r.Context()) {
				write(w, 200, map[string]string{"status": "ready"})
				return
			}
			write(w, 503, map[string]string{"status": "not_ready", "request_id": requestID})
		default:
			if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/api/v1/" {
				if options.Business != nil {
					options.Business.ServeHTTP(w, r)
					return
				}
				write(w, 503, map[string]string{"code": "DEPENDENCY_UNAVAILABLE", "request_id": requestID})
				return
			}
			if options.WebDir != "" && (r.Method == "GET" || r.Method == "HEAD") {
				if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/admin" || r.URL.Path == "/setup/admin" || r.URL.Path == "/setup" {
					if _, err := os.Stat(filepath.Join(options.WebDir, "index.html")); err == nil {
						w.Header().Del("Content-Type")
						http.ServeFile(w, r, filepath.Join(options.WebDir, "index.html"))
						return
					}
				}
				if len(r.URL.Path) > 8 && r.URL.Path[:8] == "/assets/" {
					w.Header().Del("Content-Type")
					http.StripPrefix("/assets/", http.FileServer(http.Dir(filepath.Join(options.WebDir, "assets")))).ServeHTTP(w, r)
					return
				}
			}
			write(w, 404, map[string]string{"code": "NOT_FOUND", "request_id": requestID})
		}
	})
}

func write(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
