//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/idempotency"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
)

// Two managers sharing Redis model two gateway replicas. The in-flight lock
// must prevent the second replica from calling the provider, and the completed
// response must be replayable from Redis even when the local store is empty.
func TestIdempotencyRedisCrossReplica(t *testing.T) {
	client := newRedisClient(t)
	store := redisstore.NewStatic(client)
	cfg := idempotency.Defaults()
	cfg.WaitTimeout = 150 * time.Millisecond

	replicaA := idempotency.New(cfg, store)
	replicaB := idempotency.New(cfg, store)

	key := idempotency.Key("tenant-a", "/v1/chat/completions", fmt.Sprintf("itest-%d", time.Now().UnixNano()))
	fingerprint := idempotency.Fingerprint([]byte(`{"model":"gpt-4o"}`))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Del(ctx, "gw:idempotency:data:"+key, "gw:idempotency:lock:"+key).Err()
	})

	ctx := context.Background()
	reservation, outcome, _ := replicaA.Begin(ctx, key, fingerprint)
	if outcome != idempotency.Proceed {
		t.Fatalf("replica A begin = %v, want Proceed", outcome)
	}

	if _, outcome, _ := replicaB.Begin(ctx, key, fingerprint); outcome != idempotency.InProgress {
		t.Fatalf("replica B begin = %v, want InProgress while A owns the lock", outcome)
	}

	reservation.Complete(ctx, idempotency.Entry{Status: 200, Body: []byte(`{"ok":true}`)})

	_, outcome, entry := replicaB.Begin(ctx, key, fingerprint)
	if outcome != idempotency.Replay {
		t.Fatalf("replica B replay = %v, want Replay", outcome)
	}
	if entry.Status != 200 || string(entry.Body) != `{"ok":true}` {
		t.Fatalf("replayed entry = %+v", entry)
	}

	if _, outcome, _ := replicaB.Begin(ctx, key, "different-body"); outcome != idempotency.Conflict {
		t.Fatalf("different body = %v, want Conflict", outcome)
	}
}
