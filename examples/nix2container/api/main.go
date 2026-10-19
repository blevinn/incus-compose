package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	healthCheck := flag.Bool("healthcheck", false, "run health check against localhost:8000")
	flag.Parse()

	if *healthCheck {
		resp, err := http.Get("http://127.0.0.1:8000/health")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	cacheHost := os.Getenv("CACHE_HOST")
	if cacheHost == "" {
		cacheHost = "cache"
	}
	cachePort := os.Getenv("CACHE_PORT")
	if cachePort == "" {
		cachePort = "6379"
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cacheStatus := "disconnected"
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(cacheHost, cachePort), 500*time.Millisecond)
		if err == nil {
			defer conn.Close()
			_, _ = fmt.Fprintf(conn, "PING\r\n")
			buf := make([]byte, 16)
			_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, _ := conn.Read(buf)
			if string(buf[:n]) == "+PONG\r\n" {
				cacheStatus = "connected"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":   "nix2builder-api",
			"builder":   "nix2container",
			"cache":     cacheStatus,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	fmt.Printf("API server listening on port %s\n", port)
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Server failed: %v\n", err)
		os.Exit(1)
	}
}
