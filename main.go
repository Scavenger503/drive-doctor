// Drive Doctor: explains hard drive and SSD health reports in plain language.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/ai"
	"github.com/Scavenger503/drive-doctor/internal/server"
)

func main() {
	addr := os.Getenv("DD_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	limit := 30
	if v, err := strconv.Atoi(os.Getenv("DD_DAILY_LIMIT")); err == nil {
		limit = v
	}

	cfg := ai.ConfigFromEnv()
	if cfg.Enabled() {
		log.Printf("AI explanations on: %s via %s", cfg.Model, cfg.BaseURL)
	} else {
		log.Printf("No API key set: running with rule-based explanations only")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(cfg, limit).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	log.Printf("Drive Doctor listening on %s (daily AI limit per visitor: %d)", addr, limit)
	log.Fatal(srv.ListenAndServe())
}
