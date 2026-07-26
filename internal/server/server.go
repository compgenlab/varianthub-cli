// Package server implements the `cganno server` async REST annotation service.
// It exposes the same annotation engine as the CLI over HTTP: a request submits a
// locus (or an uploaded VCF) and is queued; a worker pool annotates it; the client
// polls by job id and fetches JSON results. The /v1 API is authenticated with
// HMAC bearer tokens (see auth.go); a browser form and its /ui/* endpoints are
// open. Jobs and results persist in a dedicated SQLite database (see queue.go).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/compgenlab/cganno/internal/config"
	"github.com/compgenlab/cganno/internal/engine"
	"github.com/compgenlab/cganno/internal/model"
	"github.com/compgenlab/cganno/internal/service"
	"github.com/compgenlab/cganno/internal/store"
	"github.com/compgenlab/cganno/internal/vcf"
)

// Server holds the running annotation service: the loaded config + snapshot, the
// shared annotation-cache store, the job queue, and the public-service guards
// (trusted-proxy nets for client-IP resolution and a per-IP rate limiter).
type Server struct {
	cfg     *config.Config
	snap    *config.Snapshot
	store   store.Store // annotation cache (may be nil when the cache is disabled)
	queue   *Queue
	version string

	trusted []*net.IPNet
	limiter *ipLimiter
}

// New builds a Server over an already-loaded config/snapshot, the (optional)
// annotation-cache store, and an open job queue. version is reported by /version.
func New(cfg *config.Config, snap *config.Snapshot, st store.Store, q *Queue, version string) *Server {
	q.SetMaxJobsPerIP(cfg.Server.MaxJobsPerIP)
	return &Server{
		cfg:     cfg,
		snap:    snap,
		store:   st,
		queue:   q,
		version: version,
		trusted: parseCIDRs(cfg.Server.TrustedProxies),
		limiter: newIPLimiter(cfg.Server.RatePerMin, cfg.Server.RateBurst),
	}
}

// Run starts the worker pool, prints a valid API token to stdout, and serves HTTP
// on the configured endpoint until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	s.queue.StartWorkers(ctx, s.cfg.Server.Workers, s.runJob)

	// Background housekeeping: GC old jobs and evict idle rate-limiter buckets.
	ttl := s.cfg.Server.JobTTLDuration()
	s.queue.StartSweeper(ctx, ttl, sweepInterval(ttl))
	go s.limiterGC(ctx)

	if s.cfg.Server.RequireTokenForV1() {
		token, err := MintToken(s.cfg.Server.MasterKey, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("mint startup token: %w", err)
		}
		// The startup token goes to STDOUT (so it can be captured); logs go to STDERR.
		fmt.Fprintln(os.Stdout, token)
	} else {
		log.Printf("cganno server: /v1 API is OPEN (require_token=false) — no bearer token required")
	}
	log.Printf("cganno server: snapshot %q, %d worker(s); listening on http://%s",
		s.snap.Name, s.cfg.Server.Workers, s.cfg.Server.Endpoint)

	httpSrv := &http.Server{
		Addr:    s.cfg.Server.Endpoint,
		Handler: s.routes(),
		// Timeouts bound slow-client/slowloris connections. ReadTimeout is generous
		// to allow large (up to 64 MiB) VCF uploads over slow links.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		s.queue.Wait()
		return nil
	}
}

// routes builds the HTTP mux: an authenticated /v1 API, open ops endpoints
// (/healthz, /version), and — unless disabled — the browser UI and its /ui/*
// twins. Submit endpoints are wrapped with the per-IP throttle.
func (s *Server) routes() http.Handler {
	// Authenticated API surface.
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/annotations", s.handleAnnotations)
	api.Handle("POST /v1/annotate", s.throttle(http.HandlerFunc(s.handleAnnotateLocus)))
	api.Handle("POST /v1/annotate/vcf", s.throttle(http.HandlerFunc(s.handleAnnotateVCF)))
	api.HandleFunc("GET /v1/jobs", s.handleListJobs)
	api.HandleFunc("GET /v1/jobs/{id}", s.handleJob)
	api.HandleFunc("GET /v1/jobs/{id}/results", s.handleJobResults)

	mux := http.NewServeMux()
	// /v1 is bearer-token authenticated unless require_token=false (an open,
	// tokenless public API). Throttle + fair queue + tool-gate still apply.
	var apiHandler http.Handler = api
	if s.cfg.Server.RequireTokenForV1() {
		apiHandler = requireToken(s.cfg.Server.MasterKey, api)
	}
	mux.Handle("/v1/", apiHandler)

	// Open ops endpoints (no token) — health checks and version.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /version", s.handleVersion)

	// Browser UI + its /ui/* twins (the form page and the endpoints its JS calls).
	// Disabled entirely when ui_enabled=false; optionally token-gated when
	// ui_require_token=true (for programmatic /ui callers behind a proxy).
	if s.cfg.Server.UIIsEnabled() {
		ui := http.NewServeMux()
		ui.HandleFunc("GET /{$}", s.handleForm)
		ui.HandleFunc("GET /ui/annotations", s.handleAnnotations)
		ui.Handle("POST /ui/submit", s.throttle(http.HandlerFunc(s.handleAnnotateLocus)))
		ui.Handle("POST /ui/submit/vcf", s.throttle(http.HandlerFunc(s.handleAnnotateVCF)))
		ui.HandleFunc("GET /ui/jobs", s.handleListJobs)
		ui.HandleFunc("GET /ui/jobs/{id}", s.handleJob)
		ui.HandleFunc("GET /ui/jobs/{id}/results", s.handleJobResults)

		var uiHandler http.Handler = ui
		if s.cfg.Server.UIRequireToken {
			uiHandler = requireToken(s.cfg.Server.MasterKey, ui)
		}
		mux.Handle("/", uiHandler)
	}
	return mux
}

// throttle wraps h with the per-IP submit rate limiter (429 when exceeded). It
// keys on the trusted-proxy-resolved client IP.
func (s *Server) throttle(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(clientIP(r, s.trusted)) {
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded — slow down")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// sweepInterval picks a GC sweep cadence from the retention window: ttl/10, bounded
// to [1m, 1h]. Zero ttl (GC disabled) is handled by StartSweeper.
func sweepInterval(ttl time.Duration) time.Duration {
	iv := ttl / 10
	if iv < time.Minute {
		iv = time.Minute
	}
	if iv > time.Hour {
		iv = time.Hour
	}
	return iv
}

// limiterGC periodically evicts idle rate-limiter buckets until ctx is cancelled.
func (s *Server) limiterGC(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.limiter.gc(30 * time.Minute)
		}
	}
}

// runJob is the queue Runner: it parses the job's input into loci, resolves the
// requested annotation selection, annotates over the shared locus path, and
// returns the JSON result array (engine.BuildVariants — identical to the CLI's
// --format json). The number of variants is the locus count (per ALT allele).
func (s *Server) runJob(ctx context.Context, job Job, input []byte) ([]byte, int, error) {
	loci, err := lociFromInput(job.Kind, input)
	if err != nil {
		return nil, 0, err
	}
	selected, err := resolveSelection(s.snap, job.Selection)
	if err != nil {
		return nil, 0, err
	}
	names := annotationNames(selected)

	// A VCF upload is a bulk request: tools run over the whole input directly
	// (per-locus cache skipped) and the loci are chunked across cores. A
	// single-locus job keeps the cache and needs no chunking.
	var res engine.AnnotateResult
	if job.Kind == KindVCF {
		res, err = service.AnnotateLociChunked(ctx, s.cfg, s.snap, s.store, selected, loci,
			true, s.cfg.Server.ChunkSize(), s.cfg.Server.AnnotateThreads)
	} else {
		res, err = service.AnnotateLoci(ctx, s.cfg, s.snap, s.store, selected, loci, false)
	}
	if err != nil {
		return nil, 0, err
	}
	out, err := json.Marshal(engine.BuildVariants(loci, names, res))
	if err != nil {
		return nil, 0, err
	}
	return out, len(loci), nil
}

// lociFromInput turns a job's stored input body into loci: a single locus string
// for a locus job, or a parsed VCF for a vcf job (multi-allelic ALTs split, in
// file order).
func lociFromInput(kind string, input []byte) ([]model.Locus, error) {
	switch kind {
	case KindLocus:
		l, err := vcf.ParseLocus(string(input))
		if err != nil {
			return nil, err
		}
		return []model.Locus{l}, nil
	case KindVCF:
		return vcf.Read(bytes.NewReader(input))
	default:
		return nil, fmt.Errorf("unknown job kind %q", kind)
	}
}

// --- small HTTP helpers ----------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
