// Package config loads broker configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for the broker. Every field is
// sourced from an environment variable; see Load for names and defaults.
type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	JWTSigningKey     []byte
	AccessTTL         time.Duration
	VisibilityTimeout time.Duration
	MaxFetch          int
	MaxPayloadBytes   int
	ListenChannel     string
	MigrationsDir     string
	AuthRatePerMin    int // per-IP /auth/token requests per minute; 0 disables
}

// Load reads configuration from the environment, applying defaults for
// optional values and validating required ones.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:          getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		AccessTTL:         30 * time.Minute,
		VisibilityTimeout: 30 * time.Second,
		MaxFetch:          100,
		MaxPayloadBytes:   262144, // 256 KiB
		ListenChannel:     getenv("LISTEN_CHANNEL", "new_message"),
		MigrationsDir:     getenv("MIGRATIONS_DIR", "migrations"),
		AuthRatePerMin:    60,
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	key := os.Getenv("JWT_SIGNING_KEY")
	if len(key) < 32 {
		return nil, fmt.Errorf("JWT_SIGNING_KEY is required and must be at least 32 bytes")
	}
	c.JWTSigningKey = []byte(key)

	var err error
	if v := os.Getenv("ACCESS_TTL"); v != "" {
		if c.AccessTTL, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("ACCESS_TTL: %w", err)
		}
	}
	if v := os.Getenv("VISIBILITY_TIMEOUT"); v != "" {
		if c.VisibilityTimeout, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("VISIBILITY_TIMEOUT: %w", err)
		}
	}
	if v := os.Getenv("MAX_FETCH"); v != "" {
		if c.MaxFetch, err = strconv.Atoi(v); err != nil || c.MaxFetch <= 0 {
			return nil, fmt.Errorf("MAX_FETCH must be a positive integer")
		}
	}
	if v := os.Getenv("MAX_PAYLOAD_BYTES"); v != "" {
		if c.MaxPayloadBytes, err = strconv.Atoi(v); err != nil || c.MaxPayloadBytes <= 0 {
			return nil, fmt.Errorf("MAX_PAYLOAD_BYTES must be a positive integer")
		}
	}
	if v := os.Getenv("AUTH_RATE_PER_MIN"); v != "" {
		if c.AuthRatePerMin, err = strconv.Atoi(v); err != nil || c.AuthRatePerMin < 0 {
			return nil, fmt.Errorf("AUTH_RATE_PER_MIN must be a non-negative integer")
		}
	}

	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
