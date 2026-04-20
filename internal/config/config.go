// Package config parses environment variables into a typed Config struct.
// All knobs are optional except ZOT_REGISTRY_URL.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config carries all runtime settings. Populated exclusively from environment
// variables by FromEnv; no flag parsing, no config file, no defaults file.
type Config struct {
	// ZotRegistryURL is the full URL (scheme://host[:port]) of the Zot registry.
	// Required. Scheme determines HTTP vs HTTPS transport.
	ZotRegistryURL string

	// ZotUsername / ZotPassword are HTTP Basic credentials for Zot, if
	// accessControl requires authenticated reads. Empty → anonymous.
	ZotUsername string
	ZotPassword string

	// ZotInsecure skips TLS certificate verification for HTTPS Zot endpoints.
	// Intended only for self-signed dev registries.
	ZotInsecure bool

	// RateLimitMS is the delay in milliseconds between sequential Zot manifest
	// requests. Prevents overwhelming Zot and upstream registries.
	RateLimitMS int

	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel string

	// NamespaceInclude, if non-empty, restricts the scan to the listed namespaces.
	// Applied before NamespaceExclude.
	NamespaceInclude []string

	// NamespaceExclude always filters matching namespaces out of the scan.
	NamespaceExclude []string

	// ScanTimeout is the overall context deadline for a single run.
	ScanTimeout time.Duration
}

// FromEnv reads every supported environment variable and returns a populated
// Config. Returns an error only for required-variable absence or malformed
// values; optional variables silently fall back to sensible defaults.
func FromEnv() (*Config, error) {
	c := &Config{
		ZotRegistryURL:   os.Getenv("ZOT_REGISTRY_URL"),
		ZotUsername:      os.Getenv("ZOT_USERNAME"),
		ZotPassword:      os.Getenv("ZOT_PASSWORD"),
		RateLimitMS:      250,
		LogLevel:         "info",
		ScanTimeout:      5 * time.Minute,
		NamespaceInclude: splitCSV(os.Getenv("NAMESPACE_INCLUDE")),
		NamespaceExclude: splitCSV(os.Getenv("NAMESPACE_EXCLUDE")),
	}

	if c.ZotRegistryURL == "" {
		return nil, errors.New("ZOT_REGISTRY_URL is required")
	}
	u, err := url.Parse(c.ZotRegistryURL)
	if err != nil {
		return nil, fmt.Errorf("ZOT_REGISTRY_URL is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("ZOT_REGISTRY_URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("ZOT_REGISTRY_URL must include a host")
	}

	if v := os.Getenv("ZOT_INSECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("ZOT_INSECURE must be a boolean (true/false): %w", err)
		}
		c.ZotInsecure = b
	}

	if v := os.Getenv("RATE_LIMIT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("RATE_LIMIT_MS must be a non-negative integer, got %q", v)
		}
		c.RateLimitMS = n
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		lvl := strings.ToLower(v)
		switch lvl {
		case "debug", "info", "warn", "error":
			c.LogLevel = lvl
		default:
			return nil, fmt.Errorf("LOG_LEVEL must be one of debug/info/warn/error, got %q", v)
		}
	}

	if v := os.Getenv("SCAN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("SCAN_TIMEOUT must be a Go duration (e.g. 5m, 300s): %w", err)
		}
		if d <= 0 {
			return nil, errors.New("SCAN_TIMEOUT must be positive")
		}
		c.ScanTimeout = d
	}

	return c, nil
}

// SlogLevel maps the string log level to a slog.Level.
func (c *Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// splitCSV parses a comma-separated list, trimming whitespace and dropping
// empty entries. Returns nil for empty input so callers can treat nil as
// "no filter".
func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
