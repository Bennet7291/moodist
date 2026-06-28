// Package server provides the HTTP server for Moodist.
package server

import (
	"compress/gzip"
	"context"
	"embed"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config holds the server configuration.
type Config struct {
	// Addr is the TCP address to listen on, e.g. ":8080".
	Addr string

	// ReadTimeout caps the entire request (headers + body).
	ReadTimeout time.Duration

	// WriteTimeout caps the response write (headers + body).
	WriteTimeout time.Duration

	// IdleTimeout caps keep-alive connections waiting for a new request.
	IdleTimeout time.Duration

	// ShutdownGracePeriod is how long Shutdown waits for in-flight requests.
	ShutdownGracePeriod time.Duration

	// Logger is used for structured logging. Defaults to slog.Default().
	Logger *slog.Logger
}

// DefaultConfig returns a production-ready Config.
func DefaultConfig() Config {
	return Config{
		Addr:                ":8080",
		ReadTimeout:         5 * time.Second,
		WriteTimeout:        30 * time.Second,
		IdleTimeout:         120 * time.Second,
		ShutdownGracePeriod: 15 * time.Second,
		Logger:              slog.Default(),
	}
}

// Server is the Moodist HTTP server.
type Server struct {
	cfg Config
	srv *http.Server
	log *slog.Logger

	mu      sync.Mutex
	started bool
}

// New builds a Server serving the embedded static files.
// staticFiles must contain a sub-directory "dist" that is the Astro build output.
func New(staticFiles embed.FS, cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	// Strip the leading "dist/" prefix so that "/" maps to "dist/index.html".
	distFS, err := fs.Sub(staticFiles, "dist")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	s := &Server{cfg: cfg, log: cfg.Logger}

	// Health / readiness probe — lightweight, no body required by most
	// orchestrators, but we return a tiny JSON payload for convenience.
	mux.HandleFunc("GET /health", s.handleHealth)

	// All other requests are served from the embedded static file system.
	// Unknown paths fall through to index.html (SPA client-side routing).
	mux.Handle("/", s.staticHandler(distFS))

	s.srv = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.middlewareChain(mux),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	return s, nil
}

// ListenAndServe starts the HTTP server and blocks until it returns an error.
// Call Shutdown to stop it gracefully.
func (s *Server) ListenAndServe() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}

	s.log.Info("moodist listening", "addr", ln.Addr().String())
	return s.srv.Serve(ln)
}

// Shutdown gracefully drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownGracePeriod)
	defer cancel()
	s.log.Info("moodist shutting down")
	return s.srv.Shutdown(ctx)
}

// ─── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

// staticHandler serves files from the embedded FS and falls back to index.html
// for any path that doesn't exist (enabling client-side SPA routing).
func (s *Server) staticHandler(files fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)

		// Detect whether the requested path exists in the embedded FS.
		f, err := files.Open(strings.TrimPrefix(p, "/"))
		if err == nil {
			fi, statErr := f.Stat()
			_ = f.Close()

			// If it's a directory try index.html inside it.
			if statErr == nil && fi.IsDir() {
				index := path.Join(strings.TrimPrefix(p, "/"), "index.html")
				if _, err2 := files.Open(index); err2 == nil {
					// Delegate to the standard file server.
					addCacheHeaders(w, p)
					fileServer.ServeHTTP(w, r)
					return
				}
			}

			// Real file — serve it.
			addCacheHeaders(w, p)
			fileServer.ServeHTTP(w, r)
			return
		}

		// Path not found → serve index.html for SPA routing.
		idx, err2 := files.Open("index.html")
		if err2 != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer idx.Close()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, idx)
	})
}

// addCacheHeaders sets long-lived cache headers for fingerprinted assets
// (paths containing a content hash) and a short cache for everything else.
func addCacheHeaders(w http.ResponseWriter, p string) {
	ext := filepath.Ext(p)
	ct := mime.TypeByExtension(ext)

	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	// Astro fingerprints asset filenames — treat anything under /_astro/ as
	// immutable. All other files (index.html, manifest, service-worker …)
	// get a short cache so updates propagate quickly.
	if strings.HasPrefix(p, "/_astro/") ||
		strings.HasPrefix(p, "/assets/pwa/") ||
		strings.HasPrefix(p, "/fonts/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	}
}

// ─── middleware ───────────────────────────────────────────────────────────────

// middlewareChain composes logging → recovery → security headers → gzip.
func (s *Server) middlewareChain(next http.Handler) http.Handler {
	return s.loggingMiddleware(
		s.recoveryMiddleware(
			s.securityHeadersMiddleware(
				gzipMiddleware(next),
			),
		),
	)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.statusCode,
			"duration", time.Since(start).String(),
			"ip", realIP(r),
		)
	})
}

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "panic", rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// gzipMiddleware transparently compresses responses when the client accepts it.
func gzipMiddleware(next http.Handler) http.Handler {
	compressible := map[string]bool{
		"text/html":              true,
		"text/css":               true,
		"text/javascript":        true,
		"application/javascript": true,
		"application/json":       true,
		"image/svg+xml":          true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		ext := filepath.Ext(r.URL.Path)
		ct := mime.TypeByExtension(ext)
		base := strings.Split(ct, ";")[0]

		if !compressible[base] {
			next.ServeHTTP(w, r)
			return
		}

		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		defer gz.Close()

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")

		grw := &gzipResponseWriter{ResponseWriter: w, writer: gz}
		next.ServeHTTP(grw, r)
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.writer.Write(b)
}

// realIP extracts the originating IP, respecting common proxy headers.
func realIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.Split(fwd, ",")[0]
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// EnvOrDefault returns the value of the named environment variable, or def.
func EnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
