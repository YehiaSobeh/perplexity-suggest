// Package config loads service configuration from environment variables.
// Defaults are chosen so the service runs out-of-the-box with no .env file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// HTTP server
	Port int // PORT, default 8080

	// Upstream WebSocket
	WebSocketURL string        // PERPLEXITY_WS_URL
	Origin       string        // ORIGIN — sent as WebSocket Origin header
	UserAgent    string        // USER_AGENT
	DialTimeout  time.Duration // WS_DIAL_TIMEOUT, default 10s

	// Request handling
	RequestTimeout time.Duration // REQUEST_TIMEOUT, default 5s
	MaxRetries     int           // MAX_RETRIES, default 2

	// Reconnect
	ReconnectDelay        time.Duration // RECONNECT_DELAY, default 2s
	MaxReconnectAttempts  int           // MAX_RECONNECT_ATTEMPTS, default 5

	// Logging
	LogLevel string // LOG_LEVEL: debug|info|warn|error, default "info"
}

func Load() (Config, error) {
	cfg := Config{
		Port:                 envInt("PORT", 8080),
		WebSocketURL:         envStr("PERPLEXITY_WS_URL", "wss://suggest.perplexity.ai/suggest/ws"),
		Origin:               envStr("ORIGIN", "https://www.perplexity.ai"),
		UserAgent:            envStr("USER_AGENT", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"),
		DialTimeout:          envDur("WS_DIAL_TIMEOUT", 10*time.Second),
		RequestTimeout:       envDur("REQUEST_TIMEOUT", 5*time.Second),
		MaxRetries:           envInt("MAX_RETRIES", 2),
		ReconnectDelay:       envDur("RECONNECT_DELAY", 2*time.Second),
		MaxReconnectAttempts: envInt("MAX_RECONNECT_ATTEMPTS", 5),
		LogLevel:             envStr("LOG_LEVEL", "info"),
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return cfg, fmt.Errorf("invalid PORT: %d", cfg.Port)
	}
	if cfg.RequestTimeout <= 0 {
		return cfg, fmt.Errorf("REQUEST_TIMEOUT must be > 0")
	}
	return cfg, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
