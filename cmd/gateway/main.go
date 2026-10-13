// Package main implements the all2api gateway entry point.
package main

import (
	"context"
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
			"WARNING: api_key is not configured — running in OPEN MODE.\n" +
			"         The panel and /api/* (including GET /api/config raw config) have NO\n" +
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
		fmt.Fprintf(w, `{"status":"ok","service":"all2api","version":"%s","uptime":"%s"}`, version, time.Since(start).String())
	})

	// Router endpoint placeholder
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler)

	// Responses API (feature flag: disabled in MVP, enabled in M1)
	mux.HandleFunc("/v1/responses", responsesHandler)

	// Runtime control plane: per-source circuit breakers (fed by the frozen
	// sources.<id>.throttle.backoff.* keys) + the shared account pool. The
	// recover endpoint acts on this host — see internal/api/recover.go.
	gwHost := host.New(cfg)

	// Panel API surface (G3 consent gate + config delivery + recover wiring).
	api.Register(mux, gwHost)

	// Embedded panel with runtime __ALL2API_BASE__ injection (G5) — M1 queue ②.
	// Registered last: "/" is the SPA fallback catch-all, specific patterns win.
	mux.Handle("/", web.NewPanelHandler())

	fmt.Printf("All2API gateway v%s (%s) listening on %s\n", version, commit, addr)
	fmt.Printf("Registered adapters:\n")
	for _, a := range adapter.Default.GetAll() {
		fmt.Printf("  - %s (capabilities: 0x%x)\n", a.Name(), a.Capabilities().Value)
	}

	server := &http.Server{
		Addr:     addr,
		Handler:  mux,
		ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGTERM/SIGINT — ≤10s total
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		cancel()
	}()

	if err := server.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "changed") {
		fmt.Printf("Server error: %v\n", err)
		os.Exit(1)
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

// chatCompletionsHandler / responsesHandler — restored scaffold stubs (lost
// in the c49601c main.go rewrite, which left the mux references dangling and
// broke the build). The /v1 router layer is M2 scope: adapters expose no
// upstream-send capability yet, so these endpoints honestly report 501
// instead of faking a proxy.
func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: router layer implementation
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprint(w, `{"error":"chat completions not yet implemented"}`)
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	// Feature flag controlled by config.router.responses_api.enabled;
	// even with the flag on the upstream layer is not implemented yet.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprint(w, `{"error":"responses API not yet implemented"}`)
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
