// Command better-red-broker is the small relay behind Better Red's partner sharing.
//
// It is a dumb cache: it signs pair ids without storing them, keeps end-to-end encrypted blobs
// that her phone writes (and re-writes whenever they're missing), and hands them to whoever
// presents a valid one-time code. It cannot read the blobs.
// See docs/ for the design, the protocol and how to run it.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	key, err := loadOrCreateKey(cfg.KeyFile)
	if err != nil {
		log.Fatalf("server key: %v", err)
	}
	store, err := OpenStore(cfg.DBPath, cfg.Readers)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	api := NewAPI(cfg, store, Signer{key: key})
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newHandler(cfg, api),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go runCleanup(ctx, cfg, api)
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				api.general.sweep(now)
				api.mint.sweep(now)
				api.badAuth.sweep(now)
			}
		}
	}()

	go func() {
		log.Printf("better-red-broker %s listening on %s (max %d days, stale after %d days)", version, cfg.Addr, cfg.MaxDays, cfg.StaleDays)
		if cfg.Password != "" {
			log.Print("API password on: clients must send it (X-Better-Red-Password)")
		}
		if !cfg.RateLimit {
			log.Print("rate limits OFF: the reverse proxy must limit requests")
		} else {
			log.Printf("rate limits on; client IP from %s when the peer is one of %d trusted proxy ranges", cfg.ClientIP, len(cfg.Proxies))
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Print("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}

// runCleanup runs the clean-up job at start and then every cfg.CleanupEvery.
func runCleanup(ctx context.Context, cfg Config, api *API) {
	run := func() {
		staleBefore := api.now().AddDate(0, 0, -cfg.StaleDays)
		oldest, _ := api.window()
		deleted, pruned, err := api.store.Cleanup(ctx, staleBefore, oldest)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("cleanup: %v", err)
			return
		}
		log.Printf("cleanup: %d stale pairs deleted, %d pairs trimmed to %s", deleted, pruned, oldest)
	}
	run()
	t := time.NewTicker(cfg.CleanupEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// newHandler wires the API and the website, with the protections every request gets.
func newHandler(cfg Config, api *API) http.Handler {
	mux := http.NewServeMux()
	api.Routes(mux)
	site := newSite(cfg)
	mux.HandleFunc("GET /{$}", site.index)
	mux.Handle("GET /static/", site.static())
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /v1/\n"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic: %v", v)
				http.Error(w, "server error", http.StatusInternalServerError)
			}
		}()
		if !api.general.allow(api.ips.of(r), start) {
			api.fail(w, http.StatusTooManyRequests, "slow down")
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		// Privacy: no IPs, no pair ids (they travel in headers) in the log. Only what and how it went.
		log.Printf("%s %s %d %s", r.Method, routeName(r), rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// routeName is the path for logs, shortened.
func routeName(r *http.Request) string {
	if len(r.URL.Path) > 64 {
		return r.URL.Path[:64] + "…"
	}
	return r.URL.Path
}
