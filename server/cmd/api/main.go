package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"jacko/server/internal/gateway"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
	messagepg "jacko/server/internal/messages/postgres"
	"jacko/server/internal/outbox"
	"jacko/server/internal/realtime"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dsn := os.Getenv("JACKO_DATABASE_URL")
	if dsn == "" {
		return errors.New("JACKO_DATABASE_URL is required")
	}
	address := os.Getenv("JACKO_HTTP_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}

	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(startup, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(startup); err != nil {
		return err
	}

	messageService := messages.NewService(messagepg.NewStore(pool))
	auth := sessions.NewService(sessionpg.NewStore(pool))
	mux := http.NewServeMux()
	mux.Handle("/", httpapi.New(messageService, auth))

	if redisURL := os.Getenv("JACKO_REDIS_URL"); redisURL != "" {
		options, err := redis.ParseURL(redisURL)
		if err != nil {
			return errors.New("invalid JACKO_REDIS_URL")
		}
		options.ContextTimeoutEnabled = true
		client := redis.NewClient(options)
		defer client.Close()
		lifetime, stopRealtime := context.WithCancel(ctx)
		var workers sync.WaitGroup
		defer func() {
			stopRealtime()
			workers.Wait()
		}()

		hub := realtime.NewHub()
		broker := realtime.NewRedis(client, os.Getenv("JACKO_REDIS_TOPIC"))
		worker := outbox.NewWorker(pool, broker)
		workers.Go(func() { broker.Run(lifetime, hub) })
		workers.Go(func() { worker.Run(lifetime) })
		mux.Handle("GET /api/v1/channels/{channel}/events", gateway.New(lifetime, hub, auth, outbox.NewJournal(pool)))
	}

	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		slog.Info("starting API", "address", address)
		result <- server.ListenAndServe()
	}()

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
