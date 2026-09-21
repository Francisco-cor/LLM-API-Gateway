package provider

import (
	"strings"
	"testing"
)

func TestProviderErrorMessageIsUsefulAndBounded(t *testing.T) {
	if got := providerErrorMessage([]byte(`{"error":{"message":"quota exceeded"}}`)); got != "quota exceeded" {
		t.Fatalf("got %q, want extracted message", got)
	}
	got := providerErrorMessage([]byte(strings.Repeat("x", maxProviderErrorMessage+100)))
	if len(got) != maxProviderErrorMessage+len("…") {
		t.Fatalf("bounded message length = %d, want %d", len(got), maxProviderErrorMessage+len("…"))
	}
}
