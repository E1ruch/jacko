package postgres_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func measureDelivery(t *testing.T, parent context.Context, servers [2]*httptest.Server, route, token string, cursor int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	const subscribers, writers, total = 32, 8, 200
	connections := make([]*websocket.Conn, subscribers)
	for i := range connections {
		conn := dialChannel(t, ctx, servers[i%len(servers)], route, token, "")
		connections[i] = conn
		defer conn.CloseNow()
		readReady(t, ctx, conn, cursor)
	}

	var submitted [total]atomic.Int64
	samples := make([]time.Duration, subscribers*total)
	failures := make(chan error, subscribers+writers)
	var workers sync.WaitGroup
	start := time.Now()
	for i, conn := range connections {
		workers.Go(func() {
			for j := range total {
				var event receivedEvent
				if err := wsjson.Read(ctx, conn, &event); err != nil {
					failures <- fmt.Errorf("subscriber %d: %w", i, err)
					cancel()
					return
				}
				received := time.Since(start)
				message, err := strconv.Atoi(event.Data.Content)
				if err != nil || message < 0 || message >= total || event.Seq != cursor+int64(j)+1 {
					failures <- fmt.Errorf("subscriber %d got unexpected event: %+v", i, event)
					cancel()
					return
				}
				samples[i*total+j] = received - time.Duration(submitted[message].Load())
			}
		})
	}
	for i := range writers {
		workers.Go(func() {
			for j := i; j < total; j += writers {
				content := strconv.Itoa(j)
				submitted[j].Store(int64(time.Since(start)))
				if err := sendHTTP(ctx, servers[i%len(servers)], route, token, "load-"+content, content); err != nil {
					failures <- fmt.Errorf("writer %d: %w", i, err)
					cancel()
					return
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	elapsed := time.Since(start)
	slices.Sort(samples)
	t.Logf("local load: %d sockets, %d writers, %d messages, %d deliveries in %s, p50=%s p95=%s p99=%s",
		subscribers, writers, total, len(samples), elapsed.Round(time.Millisecond),
		samples[(len(samples)*50+99)/100-1].Round(time.Microsecond),
		samples[(len(samples)*95+99)/100-1].Round(time.Microsecond),
		samples[(len(samples)*99+99)/100-1].Round(time.Microsecond))
}
