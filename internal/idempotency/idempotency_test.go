package idempotency

import (
	"context"
	"sync"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	cfg := Defaults()
	cfg.WaitTimeout = 500 * time.Millisecond
	return New(cfg, nil)
}

func TestBeginCompleteReplay(t *testing.T) {
	m := testManager(t)
	res, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Proceed || res == nil {
		t.Fatalf("begin = %v res=%v", outcome, res)
	}
	res.Complete(context.Background(), Entry{Status: 200, Body: []byte("ok")})

	_, outcome, entry := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Replay {
		t.Fatalf("outcome = %v, want Replay", outcome)
	}
	if entry.Status != 200 || string(entry.Body) != "ok" || entry.Fingerprint != "fp1" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestConflictOnDifferentFingerprint(t *testing.T) {
	m := testManager(t)
	res, _, _ := m.Begin(context.Background(), "k1", "fp1")
	res.Complete(context.Background(), Entry{Status: 200, Body: []byte("ok")})

	_, outcome, _ := m.Begin(context.Background(), "k1", "fp2")
	if outcome != Conflict {
		t.Fatalf("outcome = %v, want Conflict", outcome)
	}
}

func TestAbortReleasesWithoutStoring(t *testing.T) {
	m := testManager(t)
	res, _, _ := m.Begin(context.Background(), "k1", "fp1")
	res.Abort()

	res2, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Proceed || res2 == nil {
		t.Fatalf("after abort: outcome=%v res=%v", outcome, res2)
	}
	res2.Abort()
}

func TestConcurrentBeginWaitsForLeader(t *testing.T) {
	m := testManager(t)
	leader, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Proceed {
		t.Fatalf("leader outcome = %v", outcome)
	}

	started := make(chan struct{})
	done := make(chan Outcome, 1)
	go func() {
		close(started)
		_, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
		done <- outcome
	}()
	<-started
	time.Sleep(20 * time.Millisecond) // let the follower reach the local lock
	leader.Complete(context.Background(), Entry{Status: 200, Body: []byte("ok")})

	select {
	case outcome := <-done:
		if outcome != Replay {
			t.Fatalf("follower outcome = %v, want Replay", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not finish")
	}
}

func TestInProgressWhenLeaderAborts(t *testing.T) {
	m := testManager(t)
	leader, _, _ := m.Begin(context.Background(), "k1", "fp1")
	done := make(chan Outcome, 1)
	go func() {
		_, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
		done <- outcome
	}()
	time.Sleep(20 * time.Millisecond)
	leader.Abort()
	select {
	case outcome := <-done:
		if outcome != InProgress {
			t.Fatalf("outcome = %v, want InProgress", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not finish")
	}
}

func TestTTLExpiry(t *testing.T) {
	cfg := Defaults()
	cfg.TTL = 10 * time.Millisecond
	cfg.WaitTimeout = 100 * time.Millisecond
	m := New(cfg, nil)
	res, _, _ := m.Begin(context.Background(), "k1", "fp1")
	res.Complete(context.Background(), Entry{Status: 200, Body: []byte("ok")})
	time.Sleep(30 * time.Millisecond)
	_, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Proceed {
		t.Fatalf("outcome = %v, want Proceed after TTL", outcome)
	}
}

func TestOversizedEntryNotStored(t *testing.T) {
	cfg := Defaults()
	cfg.EntryMaxBytes = 16
	m := New(cfg, nil)
	res, _, _ := m.Begin(context.Background(), "k1", "fp1")
	res.Complete(context.Background(), Entry{Status: 200, Body: []byte("this body is definitely too large")})
	_, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Proceed {
		t.Fatalf("outcome = %v, want Proceed for an oversized entry", outcome)
	}
}

func TestDisabledManagerDoesNotReserve(t *testing.T) {
	cfg := Defaults()
	cfg.Enabled = false
	m := New(cfg, nil)
	res, outcome, _ := m.Begin(context.Background(), "k1", "fp1")
	if outcome != Proceed || res != nil {
		t.Fatalf("outcome=%v res=%v, want Proceed/nil", outcome, res)
	}
}

func TestKeyScopesTenantAndPath(t *testing.T) {
	a := Key("tenant-a", "/v1/chat/completions", "key")
	b := Key("tenant-b", "/v1/chat/completions", "key")
	c := Key("tenant-a", "/v1/embeddings", "key")
	if a == b || a == c || b == c {
		t.Fatal("key derivation must include tenant and path")
	}
	if Key("tenant-a", "/v1/chat/completions", "key") != a {
		t.Fatal("key derivation is not deterministic")
	}
	if Fingerprint([]byte("x")) == Fingerprint([]byte("y")) {
		t.Fatal("fingerprint collision")
	}
}

func TestConcurrentDifferentKeysDoNotBlock(t *testing.T) {
	m := testManager(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i))
			res, outcome, _ := m.Begin(context.Background(), key, "fp")
			if outcome != Proceed {
				t.Errorf("key %s outcome %v", key, outcome)
				return
			}
			res.Abort()
		}(i)
	}
	wg.Wait()
}
