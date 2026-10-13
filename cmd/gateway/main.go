package main

import (
	"fmt"
	"net/http"
	"time"

	"all2api/internal/adapter"
	_ "all2api/internal/platform/workbuddy" // Register via init()
)

func main() {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","service":"all2api","uptime":"%s"}`, time.Since(start).String())
	})

	// Router endpoint placeholder
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler)

	// Responses API (feature flag: disabled in MVP, enabled in M1)
	mux.HandleFunc("/v1/responses", responsesHandler)

	start := time.Now()
	addr := ":7863"
	fmt.Printf("All2API gateway listening on %s\n", addr)
	fmt.Printf("Registered adapters:\n")
	for _, a := range adapter.Default.GetAll() {
		fmt.Printf("  - %s (capabilities: 0x%x)\n", a.Name(), a.Capabilities().Value)
	}

	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: router layer implementation
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprint(w, `{"error":"chat completions not yet implemented"}`)
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	// Feature flag controlled by config.router.responses.enabled
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprint(w, `{"error":"responses API feature flag not enabled"}`)
}
