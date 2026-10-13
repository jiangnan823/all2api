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
	_ "all2api/internal/platform/workbuddy" // Register via init()
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

	start := time.Now()
	addr := ":7863"

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

	fmt.Printf("All2API gateway v%s (%s) listening on %s\n", version, commit, addr)
	fmt.Printf("Registered adapters:\n")
	for _, a := range adapter.Default.GetAll() {
		fmt.Printf("  - %s (capabilities: 0x%x)\n", a.Name(), a.Capabilities().Value)
	}

	server := &http.Server{
		Addr:     addr,
		Handler:  mux,
		ReadTimeOut: 5 * time.Second,
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
