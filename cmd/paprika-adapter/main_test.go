package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite docs/tools.json from the current tool definitions")

func setEnv(t *testing.T, kv ...string) {
	t.Helper()
	for _, name := range []string{"PAPRIKA_EMAIL", "PAPRIKA_PASSWORD", "PAPRIKA_EMAIL_FILE", "PAPRIKA_PASSWORD_FILE", "PAPRIKA_BASE_URL",
		"PAPRIKA_DATA_DIR", "PAPRIKA_LISTEN", "PAPRIKA_AUTH_TOKENS", "PAPRIKA_AUTH_TOKENS_FILE", "PAPRIKA_ENABLE_WRITES", "PAPRIKA_APPROVAL", "PAPRIKA_MAX_AGE", "PAPRIKA_SYNC_INTERVAL", "PAPRIKA_TZ", "PAPRIKA_LOG_LEVEL"} {
		t.Setenv(name, "")
	}
	for i := 0; i < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestConfigDefaultsAreReadOnly(t *testing.T) {
	setEnv(t, "PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "pw", "XDG_STATE_HOME", "/state")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.writes || cfg.humanOnly || cfg.maxAge != 5*time.Minute || cfg.syncInterval != 15*time.Minute || cfg.dataDir != "/state/paprika-adapter" ||
		cfg.listen != "127.0.0.1:8080" || len(cfg.clients) != 0 {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestConfigReadsSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("s3cret with spaces \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tokens, []byte("anna:"+annaToken+"\npeter:"+peterToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setEnv(t, "PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD_FILE", pw, "PAPRIKA_ENABLE_WRITES", "true", "PAPRIKA_APPROVAL", "human",
		"PAPRIKA_TZ", "America/New_York", "PAPRIKA_DATA_DIR", dir, "PAPRIKA_SYNC_INTERVAL", "0",
		"PAPRIKA_AUTH_TOKENS_FILE", tokens, "PAPRIKA_LISTEN", "0.0.0.0:9000")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.password != "s3cret with spaces " || !cfg.writes || !cfg.humanOnly || cfg.location.String() != "America/New_York" || cfg.syncInterval != 0 ||
		cfg.listen != "0.0.0.0:9000" || len(cfg.clients) != 2 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestConfigRejectsBadValuesWithoutEchoingSecrets(t *testing.T) {
	for name, kv := range map[string][]string{
		"no credentials":   {},
		"no password":      {"PAPRIKA_EMAIL", "a@b.c"},
		"missing file":     {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD_FILE", "/does/not/exist"},
		"bad bool":         {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "hunter2", "PAPRIKA_ENABLE_WRITES", "sure"},
		"bad approval":     {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "hunter2", "PAPRIKA_APPROVAL", "never"},
		"bad duration":     {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "hunter2", "PAPRIKA_MAX_AGE", "soon"},
		"negative":         {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "hunter2", "PAPRIKA_SYNC_INTERVAL", "-1m"},
		"unknown timezone": {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "hunter2", "PAPRIKA_TZ", "Mars/Olympus"},
		"weak token":       {"PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "hunter2", "PAPRIKA_AUTH_TOKENS", "anna:hunter2"},
	} {
		t.Run(name, func(t *testing.T) {
			setEnv(t, kv...)
			_, err := loadConfig()
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error echoes the password: %v", err)
			}
		})
	}
}

// docs/tools.json is the published contract. It must match what the server
// actually advertises; regenerate with: go test ./cmd/paprika-adapter -update
func TestPublishedToolSchemasAreCurrent(t *testing.T) {
	var buf bytes.Buffer
	if err := printTools(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "docs", "tools.json")
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("docs/tools.json is out of date; run: go test ./cmd/paprika-adapter -update")
	}
}
