package auth

import (
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/config"
)

func TestHashKeyPepperChangesDigest(t *testing.T) {
	plain := hashKey("same-key", "")
	peppered := hashKey("same-key", "pepper-a")
	other := hashKey("same-key", "pepper-b")

	if plain == peppered {
		t.Fatal("pepper did not change the key digest")
	}
	if peppered == other {
		t.Fatal("different peppers produced the same digest")
	}
	if len(peppered) != 64 {
		t.Fatalf("HMAC-SHA256 hex digest length = %d, want 64", len(peppered))
	}
}

func TestHasScopeFailsClosed(t *testing.T) {
	empty := &Key{}
	if empty.HasScope("chat:write") {
		t.Fatal("key without scopes was allowed chat:write")
	}
	if !empty.HasScope("") {
		t.Fatal("empty required scope should not be rejected")
	}

	limited := &Key{Scopes: []string{"models:read"}}
	if !limited.HasScope("models:read") {
		t.Fatal("models:read scope rejected")
	}
	if limited.HasScope("chat:write") {
		t.Fatal("chat:write scope unexpectedly granted")
	}

	wildcard := &Key{Scopes: []string{"*"}}
	if !wildcard.HasScope("chat:write") || !wildcard.HasScope("resource:team-a") {
		t.Fatal("wildcard scope did not grant access")
	}

	resource := &Key{Scopes: []string{"resource:team-a"}}
	if !resource.HasScope("resource:team-a") || resource.HasScope("resource:team-b") {
		t.Fatal("resource scopes are not isolated")
	}
}

func TestReloadWithPepperSwapsKeysAndPepperAtomically(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	store := NewWithPepper([]config.APIKeyConfig{{Key: "first-key", Tenant: "a"}}, "p1")
	if _, ok := store.Authenticate("first-key"); !ok {
		t.Fatal("first key did not authenticate")
	}

	store.ReloadWithPepper([]config.APIKeyConfig{{Key: "second-key", Tenant: "b"}}, "p2")
	if _, ok := store.Authenticate("first-key"); ok {
		t.Fatal("removed key still authenticates after reload")
	}
	key, ok := store.Authenticate("second-key")
	if !ok {
		t.Fatal("new key does not authenticate after reload")
	}
	if key.Tenant != "b" {
		t.Fatalf("tenant = %q, want b", key.Tenant)
	}

	// Reload keeps the pepper when called through the legacy entry point.
	store.Reload([]config.APIKeyConfig{{Key: "third-key", Tenant: "c", ExpiresAt: &expires}})
	if _, ok := store.Authenticate("third-key"); !ok {
		t.Fatal("Reload dropped the active pepper")
	}
}

func TestPlaceholderKeysAreIgnored(t *testing.T) {
	store := New([]config.APIKeyConfig{
		{Key: "${MISSING_KEY}", Tenant: "a"},
		{Key: "", Tenant: "b"},
		{Key: "real-key", Tenant: "c"},
	})
	if _, ok := store.Authenticate("${MISSING_KEY}"); ok {
		t.Fatal("unexpanded placeholder authenticated")
	}
	if _, ok := store.Authenticate(""); ok {
		t.Fatal("empty key authenticated")
	}
	if _, ok := store.Authenticate("real-key"); !ok {
		t.Fatal("real key did not authenticate")
	}
}
