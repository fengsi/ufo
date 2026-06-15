// Command api is the UFO Go control-plane server.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ufo/apps/api/internal/migrate"
	"ufo/apps/api/internal/server"
)

func main() {
	databaseURL := env("DATABASE_URL", "postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable")
	addr := env("UFO_API_ADDR", ":8080")

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := waitForDB(ctx, pool); err != nil {
		log.Fatalf("postgres not reachable: %v", err)
	}

	if err := migrate.Run(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("migrations applied")

	// Long-poll notifier: LISTEN for run-queued notifications.
	longPoll := time.Duration(envFloat("UFO_LONGPOLL_SECONDS", 25) * float64(time.Second))
	notifier := server.NewNotifier(databaseURL, "ufo_run_queued", "ufo_changed")
	notifier.Start(ctx)

	srv := server.New(pool, longPoll, notifier)
	srv.StartHub(ctx) // WebSocket fan-out of typed change events
	log.Printf("claim long-poll: %s", longPoll)

	// Start the lease sweeper (requeues runs whose rover went silent).
	leaseSeconds := envFloat("UFO_RUN_LEASE_SECONDS", 30)
	sweepInterval := time.Duration(leaseSeconds/3*float64(time.Second))
	if sweepInterval < 5*time.Second {
		sweepInterval = 5 * time.Second
	}
	srv.StartLeaseSweeper(ctx, leaseSeconds, sweepInterval)
	log.Printf("lease sweeper: lease=%.0fs interval=%s", leaseSeconds, sweepInterval)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("UFO API listening on %s", addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func waitForDB(ctx context.Context, pool *pgxpool.Pool) error {
	var lastErr error
	for i := 0; i < 30; i++ {
		if err := pool.Ping(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return lastErr
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
