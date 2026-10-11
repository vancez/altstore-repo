package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "data", "directory for config.json")
	password := flag.String("password", os.Getenv("ALTSTORE_PASSWORD"), "password for the admin UI; empty disables auth (localhost only)")
	flag.Parse()

	log.SetFlags(log.Ltime)

	store, err := NewStore(*dataDir)
	if err != nil {
		log.Fatalf("could not load config: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           NewServer(store, NewReleases(), *password),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second, // building a source can hit several repos
		IdleTimeout:       2 * time.Minute,
	}

	log.Printf("AltStore source manager listening on http://localhost%s", *addr)
	log.Printf("feeds: /source.json (all apps), /source/<group>.json")
	if *password == "" {
		log.Printf("no password set: the admin API is open. Use -password or ALTSTORE_PASSWORD to require auth")
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server exited: %v", err)
	}
}

// timeoutCtx derives a context from the request so a client that hangs up
// cancels the outbound release API calls too.
func timeoutCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
