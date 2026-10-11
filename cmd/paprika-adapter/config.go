package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"paprika-adapter/internal/mcpserver"
)

type config struct {
	email        string
	password     string
	baseURL      string
	dataDir      string
	listen       string
	clients      []clientToken
	writes       bool
	approval     mcpserver.Approval
	maxAge       time.Duration
	syncInterval time.Duration
	location     *time.Location
	logLevel     slog.Level
}

// loadConfig reads the environment. Secrets may come from NAME or from the file named by NAME_FILE.
func loadConfig() (config, error) {
	cfg := config{baseURL: os.Getenv("PAPRIKA_BASE_URL"), dataDir: os.Getenv("PAPRIKA_DATA_DIR"), location: time.Local}
	var err error
	if cfg.email, err = secret("PAPRIKA_EMAIL"); err != nil {
		return cfg, err
	}
	if cfg.password, err = secret("PAPRIKA_PASSWORD"); err != nil {
		return cfg, err
	}
	if cfg.email == "" || cfg.password == "" {
		return cfg, fmt.Errorf("PAPRIKA_EMAIL and PAPRIKA_PASSWORD (or their _FILE variants) must be set")
	}
	if cfg.dataDir == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return cfg, fmt.Errorf("set PAPRIKA_DATA_DIR: %w", err)
			}
			state = filepath.Join(home, ".local", "state")
		}
		cfg.dataDir = filepath.Join(state, "paprika-adapter")
	}
	if cfg.listen = os.Getenv("PAPRIKA_LISTEN"); cfg.listen == "" {
		cfg.listen = "127.0.0.1:8080"
	}
	tokens, err := secret("PAPRIKA_AUTH_TOKENS")
	if err != nil {
		return cfg, err
	}
	if cfg.clients, err = parseTokens(tokens); err != nil {
		return cfg, err
	}
	if cfg.writes, err = envBool("PAPRIKA_ENABLE_WRITES"); err != nil {
		return cfg, err
	}
	switch cfg.approval = mcpserver.Approval(os.Getenv("PAPRIKA_APPROVAL")); cfg.approval {
	case "":
		cfg.approval = mcpserver.ApprovalAuto
	case mcpserver.ApprovalAuto, mcpserver.ApprovalHuman, mcpserver.ApprovalNone:
	default:
		return cfg, fmt.Errorf("PAPRIKA_APPROVAL must be auto, human or none")
	}
	if cfg.maxAge, err = envDuration("PAPRIKA_MAX_AGE", 5*time.Minute); err != nil {
		return cfg, err
	}
	if cfg.syncInterval, err = envDuration("PAPRIKA_SYNC_INTERVAL", 15*time.Minute); err != nil {
		return cfg, err
	}
	if tz := os.Getenv("PAPRIKA_TZ"); tz != "" {
		if cfg.location, err = time.LoadLocation(tz); err != nil {
			return cfg, fmt.Errorf("PAPRIKA_TZ: %w", err)
		}
	}
	if lvl := os.Getenv("PAPRIKA_LOG_LEVEL"); lvl != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(lvl)); err != nil {
			return cfg, fmt.Errorf("PAPRIKA_LOG_LEVEL: %w", err)
		}
	}
	return cfg, nil
}

func secret(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	path := os.Getenv(name + "_FILE")
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

func envBool(name string) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return b, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration such as 5m", name)
	}
	return d, nil
}
