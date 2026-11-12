package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"jacko/server/internal/accounts"
	accountpg "jacko/server/internal/accounts/postgres"
	accountmail "jacko/server/internal/accounts/smtp"
	"jacko/server/internal/attachments"
	attachmentpg "jacko/server/internal/attachments/postgres"
	attachments3 "jacko/server/internal/attachments/s3"
	"jacko/server/internal/communities"
	communitypg "jacko/server/internal/communities/postgres"
	"jacko/server/internal/gateway"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
	messagepg "jacko/server/internal/messages/postgres"
	"jacko/server/internal/moderation"
	"jacko/server/internal/outbox"
	"jacko/server/internal/presence"
	"jacko/server/internal/realtime"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
	"jacko/server/internal/telemetry"
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
	ctx, stop := context.WithCancel(ctx)
	defer stop()
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
	accountService := accounts.NewService(accountpg.NewStore(pool))
	sessionStore := sessionpg.NewStore(pool)
	var delivery *accounts.Delivery
	if address := os.Getenv("JACKO_SMTP_ADDR"); address != "" {
		mailer, err := accountmail.New(address, os.Getenv("JACKO_SMTP_FROM"), os.Getenv("JACKO_SMTP_USER"), os.Getenv("JACKO_SMTP_PASSWORD"))
		if err != nil {
			return err
		}
		delivery = accounts.NewDelivery(accountService, mailer)
		sessionStore = sessionpg.NewVerifiedStore(pool)
		mailCtx, stopMail := context.WithCancel(ctx)
		var mailWorker sync.WaitGroup
		mailWorker.Go(func() { delivery.Run(mailCtx) })
		defer func() { stopMail(); mailWorker.Wait() }()
	}
	auth := sessions.NewService(sessionStore)
	metrics := telemetry.New()
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", httpapi.Readiness(pool.Ping))
	mux.Handle("/", httpapi.New(messageService, auth, metrics))
	var authLimits httpapi.RateLimiter
	var memberPresence httpapi.MemberPresence

	if endpoint := os.Getenv("JACKO_S3_URL"); endpoint != "" {
		objects, err := attachments3.New(endpoint, os.Getenv("JACKO_S3_REGION"), os.Getenv("JACKO_S3_BUCKET"),
			os.Getenv("JACKO_S3_ACCESS_KEY"), os.Getenv("JACKO_S3_SECRET_KEY"))
		if err != nil {
			return err
		}
		httpapi.RegisterAttachments(mux, attachments.NewService(attachmentpg.NewStore(pool), objects), auth, metrics)
	}

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

		hub := realtime.NewHub(metrics)
		metrics.ObserveQueueDepth(hub.QueuedEvents)
		topic := os.Getenv("JACKO_REDIS_TOPIC")
		authLimits = httpapi.NewRedisLimits(client, topic)
		broker := realtime.NewRedis(client, topic)
		tracker := presence.NewRedis(client, topic)
		memberPresence = tracker
		worker := outbox.NewWorker(pool, broker, metrics)
		workers.Go(func() { broker.Run(lifetime, hub) })
		workers.Go(func() { worker.Run(lifetime) })
		workers.Go(func() { worker.ObserveBacklog(lifetime) })
		mux.Handle("GET /api/v1/channels/{channel}/events", gateway.New(lifetime, hub, auth, outbox.NewJournal(pool), metrics, tracker))
	}

	httpapi.RegisterAuthWithOptions(mux, accountService, auth, metrics, authLimits, delivery)
	httpapi.RegisterModeration(mux, moderation.NewStore(pool), auth, metrics, authLimits)
	httpapi.RegisterCommunities(mux, communities.NewService(communitypg.NewStore(pool)), auth, memberPresence, metrics)

	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	defer server.Close()
	result := make(chan error, 2)
	if metricsAddr := os.Getenv("JACKO_METRICS_ADDR"); metricsAddr != "" {
		listener, err := net.Listen("tcp", metricsAddr)
		if err != nil {
			return fmt.Errorf("listen for metrics: %w", err)
		}
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", metrics.Handler())
		metricsServer := &http.Server{
			Handler:           metricsMux,
			ReadHeaderTimeout: 3 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		defer metricsServer.Close()
		go func() {
			slog.Info("starting metrics listener", "address", listener.Addr().String())
			result <- metricsServer.Serve(listener)
		}()
	}
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
