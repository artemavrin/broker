package config

import (
	"log/slog"
	"testing"
)

// setRequired fills the two variables Load insists on, so each case only has
// to vary the one under test.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://broker:broker@127.0.0.1:5432/broker")
	t.Setenv("JWT_SIGNING_KEY", "0123456789abcdef0123456789abcdef")
}

func TestLoadLogLevel(t *testing.T) {
	cases := []struct {
		env  string
		want slog.Level
	}{
		{"", slog.LevelInfo}, // unset: the production default
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug}, // names are case-insensitive
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for _, c := range cases {
		name := c.env
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("LOG_LEVEL", c.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LogLevel != c.want {
				t.Fatalf("LogLevel = %v, want %v", cfg.LogLevel, c.want)
			}
		})
	}
}

func TestLoadLogLevelRejectsGarbage(t *testing.T) {
	setRequired(t)
	t.Setenv("LOG_LEVEL", "chatty")
	if _, err := Load(); err == nil {
		t.Fatal("an unknown level name must fail Load, not be silently ignored")
	}
}
