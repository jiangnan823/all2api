// Package main implements the all2api gateway entry point.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/api"
	"all2api/internal/config"
	"all2api/internal/host"
	// Assembly layer for build-tag-based platform registration.
	// -tags full → all platforms; -tags workbuddy → WorkBuddy only; etc.
	_ "all2api/internal/platforms"
	"all2api/internal/router"
	"all2api/internal/web"
)

// Version injected by ldflags at build time.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}

	// Config resolution: -config flag (Dockerfile CMD) → ALL2API_CONFIG env
	// (compose/fpk) → ./config.json. A missing file is tolerated (defaults
	// from config.Load) so bare-binary runs still start.
	cfgPath := resolveConfigPath()
	if err := config.Load(cfgPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("config: %s not found, using built-in defaults\n", cfgPath)
		} else {
			fmt.Printf("config: load %s: %v\n", cfgPath, err)
			os.Exit(1)
		}
	}
	cfg := config.Shared.Get()

	// Open-mode disclosure (PM seq=461 audit point): with api_key unset the
	// /api/* surface (including the raw config passthrough) is unauthenticated.
	if strings.TrimSpace(cfg.APIKey) == "" {
		fmt.Fprintf(os.Stderr, "\n"+
			"WARNING: api_key is not configured — running in OPEN MODE.\n"+
			"         The panel and /api/* (including GET /api/config raw config) have NO\n"+
			"         authentication. Do NOT expose listen %q to an untrusted network.\n\n",
			cfg.Listen)
	}

	start := time.Now()
	addr := cfg.Listen
	if addr == "" {
		addr = ":7863"
	}

	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","service":"all2api","version":"%s","buildDate":"%s","uptime":"%s"}`, version, buildDate, time.Since(start).String())
	})

	// Runtime control plane: per-source circuit breakers (fed by the frozen
	// sources.<id>.throttle.backoff.* keys) + the shared account pool. The
	// recover endpoint acts on this host — see internal/api/recover.go.
	gwHost := host.New(cfg)

	// OpenAI-compatible router surface (/v1/chat/completions, /v1/responses).
	// Upstream sends resolve per request from adapters implementing
	// adapter.Sender; with no implementer registered every /v1 call answers
	// 503 "no upstream sender" instead of failing at assembly time.
	router.Register(mux, gwHost, adapter.Default)

	// Panel API surface (G3 consent gate + config delivery + recover wiring).
	api.Register(mux, gwHost)

	// Embedded panel with runtime __ALL2API_BASE__ injection (G5) — M1 queue ②.
	// Registered last: "/" is the SPA fallback catch-all, specific patterns win.
	mux.Handle("/", web.NewPanelHandler())

	fmt.Printf("All2API gateway v%s (%s) listening on %s\n", version, commit, addr)
	fmt.Printf("Registered adapters:\n")
	senders := 0
	for _, a := range adapter.Default.GetAll() {
		fmt.Printf("  - %s (capabilities: 0x%x)\n", a.Name(), a.Capabilities().Value)
		if _, ok := a.(adapter.Sender); ok {
			senders++
		}
	}
	fmt.Printf("Upstream senders registered: %d\n", senders)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// A2 (adversarial review): the signal channel drives server.Shutdown
	// directly. The previous code cancelled a context with no consumer while
	// ListenAndServe blocked, so the Shutdown block was unreachable and the
	// ≤10s graceful-stop contract could never hold. Exit-code path is now
	// explicit: real ListenAndServe errors exit 1, ErrServerClosed exits 0.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("Server error: %v\n", err)
			os.Exit(1)
		}
		return
	case <-sigCh:
	}

	// Shutdown with 10s timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	fmt.Println("\nShutting down...")
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Printf("Shutdown error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Shutdown complete.")
}

// resolveConfigPath implements: -config <path> flag → ALL2API_CONFIG env →
// ./config.json. Matches the devops Dockerfile CMD and compose env contract.
func resolveConfigPath() string {
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-config" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
		if strings.HasPrefix(os.Args[i], "-config=") {
			return strings.TrimPrefix(os.Args[i], "-config=")
		}
	}
	if p := os.Getenv("ALL2API_CONFIG"); p != "" {
		return p
	}
	return "./config.json"
}

// runHealthcheck performs an HTTP GET on /healthz and exits based on status.
// Used by Docker HEALTHCHECK, fpk cmd/main status, and CI smoke tests.
func runHealthcheck() {
	resp, err := http.Get("http://localhost:7863/healthz")
	if err != nil {
		fmt.Printf("healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("healthcheck failed: status %d\n", resp.StatusCode)
		os.Exit(1)
	}

	fmt.Println("healthcheck OK")
	os.Exit(0)
}
