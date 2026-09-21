package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/config"
)

func TestConfig_LoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  port: 8080
  read_timeout: 1s
  write_timeout: 1s
providers:
  openai:
    api_key: test-key
    base_url: http://localhost
    timeout: 1s
    models: [gpt-4o]
unknown_setting: true
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("config with unknown field was accepted")
	}
}
