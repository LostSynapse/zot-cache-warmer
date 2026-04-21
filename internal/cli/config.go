// Package cli holds defaults and helpers shared between the cluster CronJob
// (cmd/zot-cache-warmer) and the standalone CLI (cmd/zot-warm). Both binaries
// MUST read defaults from here rather than duplicating values, so a change
// to the canonical registry map ships uniformly across both surfaces.
//
// This file defines the Viper-backed configuration loader used by the
// standalone CLI. The cluster CronJob keeps its env-only config in
// internal/config/config.go because k8s CronJob env spec is the canonical
// delivery mechanism for that binary.
package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// CLIConfig is the runtime configuration for the standalone zot-warm CLI.
// Populated from (in decreasing precedence): command-line flags, environment
// variables, config file, built-in defaults.
type CLIConfig struct {
	// ZotURL is the full URL (scheme://host[:port]) of the Zot registry.
	ZotURL string

	// ZotUsername / ZotPassword are HTTP Basic credentials for Zot.
	// Empty username = anonymous.
	ZotUsername string
	ZotPassword string

	// ZotInsecure skips TLS certificate verification. Self-signed dev only.
	ZotInsecure bool

	// RegistryMap is upstream-registry → Zot-local-path-prefix.
	RegistryMap map[string]string

	// RateLimitMS is the delay between sequential Zot manifest requests.
	RateLimitMS int

	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel string

	// ScanTimeout is the overall context deadline for a single run.
	ScanTimeout time.Duration

	// Quiet suppresses INFO log lines (per-image processing); WARN, ERROR,
	// and the run-complete summary are always emitted.
	Quiet bool

	// Soft changes exit-code behavior. When true, per-image warm failures do
	// NOT trigger non-zero exit; only hard failures (bad input, network
	// unreachable, auth rejected) do. When false (default / strict), any
	// warm failure exits non-zero — suitable for CI gates that must block a
	// deploy if the cache cannot be primed.
	Soft bool

	// Force treats the positional argument as a file path even if the file
	// does not yet exist on disk. Useful for scripted callers that want a
	// clear "file not found" error rather than auto-interpretation as an
	// image reference.
	Force bool
}

// RegisterFlags attaches the CLI flags to the given pflag.FlagSet. Call
// Load() after flag parsing to produce a populated CLIConfig.
func RegisterFlags(fs *pflag.FlagSet) {
	// Named, long-form flags only — short single-char flags are avoided so
	// the surface stays explicit in scripted usage and no collision with
	// future additions is possible.
	fs.String("zot-url", DefaultZotURL, "Zot registry URL (scheme://host[:port])")
	fs.String("zot-username", "", "Zot HTTP Basic auth username (empty = anonymous)")
	fs.String("zot-password", "", "Zot HTTP Basic auth password (prefer ZOT_PASSWORD env var)")
	fs.Bool("zot-insecure", false, "Skip TLS certificate verification (dev only)")
	fs.StringToString("registry-map", DefaultRegistryMap, "Upstream registry → Zot destination (docker.io=docker-images,...)")
	fs.Int("rate-limit-ms", DefaultRateLimitMS, "Delay between sequential Zot requests (milliseconds)")
	fs.String("log-level", DefaultLogLevel, "Log level: debug, info, warn, error")
	fs.Duration("scan-timeout", DefaultScanTimeout, "Overall run budget (e.g. 15m)")
	fs.Bool("quiet", false, "Suppress INFO lines; keep WARN/ERROR/summary")
	fs.Bool("soft", false, "Warm failures don't trigger non-zero exit (opportunistic mode)")
	fs.Bool("file", false, "Treat positional argument as a file path, even if it doesn't exist on disk")
}

// Load materializes a CLIConfig from the already-parsed flag set, merging in
// environment variables and config file values. Precedence:
//
//  1. command-line flags (highest)
//  2. environment variables: ZOT_WARM_<UPPER_FLAG> with dashes → underscores
//     e.g. --zot-url → ZOT_WARM_ZOT_URL
//     Note: the special aliases ZOT_REGISTRY_URL, ZOT_USERNAME, ZOT_PASSWORD,
//     ZOT_REGISTRY_MAP are also honored for compatibility with the cluster
//     CronJob's env vars, so operators can reuse a single Secret.
//  3. config file values: first of
//        ./zot-warm.yaml
//        ./config.yaml
//        $XDG_CONFIG_HOME/zot-warm/config.yaml (or $HOME/.config/zot-warm/config.yaml)
//        /etc/zot-warm/config.yaml
//  4. built-in defaults (lowest)
//
// Returns an error for invalid values (bad log level, malformed registry
// map, etc.). Missing config files are silently ignored; unreadable ones
// produce a warning and fall through to env + defaults.
func Load(fs *pflag.FlagSet) (*CLIConfig, error) {
	v := viper.New()

	// Bind flags first so they take priority.
	if err := v.BindPFlags(fs); err != nil {
		return nil, fmt.Errorf("bind flags: %w", err)
	}

	// Environment variables. Two naming schemes are honored:
	//   1. ZOT_WARM_<UPPER>        (automatic via SetEnvPrefix + key replacer)
	//   2. ZOT_REGISTRY_URL, ZOT_USERNAME, ZOT_PASSWORD, ZOT_REGISTRY_MAP
	//      (aliases so CI can share one secret with the cluster CronJob)
	v.SetEnvPrefix("ZOT_WARM")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	// Compatibility aliases — same Secret for cluster and CI. BindEnv with
	// multiple names makes Viper check each in order when resolving the key.
	_ = v.BindEnv("zot-url", "ZOT_WARM_ZOT_URL", "ZOT_REGISTRY_URL")
	_ = v.BindEnv("zot-username", "ZOT_WARM_ZOT_USERNAME", "ZOT_USERNAME")
	_ = v.BindEnv("zot-password", "ZOT_WARM_ZOT_PASSWORD", "ZOT_PASSWORD")
	_ = v.BindEnv("registry-map", "ZOT_WARM_REGISTRY_MAP", "ZOT_REGISTRY_MAP")

	// Config file resolution: try a ranked list of candidates and use the
	// first that exists. We do this manually rather than with Viper's
	// built-in AddConfigPath because we want two different file NAMES
	// (zot-warm.yaml and config.yaml) with separate search orders.
	if path := findConfigFile(); path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				fmt.Fprintf(os.Stderr, "warning: could not read config file %q: %v\n", path, err)
			}
		}
	}

	// Registry map needs special handling: Viper can't coerce StringToString
	// from an env-var "key=val,key=val" string into a map directly. If the
	// source was env, we'll have a string; if it was flag or file, a map.
	regMap, err := resolveRegistryMap(v)
	if err != nil {
		return nil, err
	}

	c := &CLIConfig{
		ZotURL:      v.GetString("zot-url"),
		ZotUsername: v.GetString("zot-username"),
		ZotPassword: v.GetString("zot-password"),
		ZotInsecure: v.GetBool("zot-insecure"),
		RegistryMap: regMap,
		RateLimitMS: v.GetInt("rate-limit-ms"),
		LogLevel:    strings.ToLower(v.GetString("log-level")),
		ScanTimeout: v.GetDuration("scan-timeout"),
		Quiet:       v.GetBool("quiet"),
		Soft:        v.GetBool("soft"),
		Force:       v.GetBool("file"),
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// findConfigFile returns the first existing config file from the ranked
// search path, or "" if none exists. Callers can check emptiness rather
// than relying on Viper's NotFound error type.
func findConfigFile() string {
	var candidates []string
	// Project-local (highest).
	candidates = append(candidates, "./zot-warm.yaml", "./config.yaml")
	// User-level.
	if h := xdgConfigHome(); h != "" {
		candidates = append(candidates, filepath.Join(h, "zot-warm", "config.yaml"))
	}
	// System-wide (lowest).
	candidates = append(candidates, "/etc/zot-warm/config.yaml")

	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// Validate checks the post-load config for internal consistency. Centralized
// so both callers (Load, and anyone constructing a CLIConfig programmatically)
// go through the same checks.
func (c *CLIConfig) Validate() error {
	if c.ZotURL == "" {
		return fmt.Errorf("zot-url is required")
	}
	if !strings.HasPrefix(c.ZotURL, "http://") && !strings.HasPrefix(c.ZotURL, "https://") {
		return fmt.Errorf("zot-url must start with http:// or https://, got %q", c.ZotURL)
	}
	if c.RateLimitMS < 0 {
		return fmt.Errorf("rate-limit-ms must be >= 0, got %d", c.RateLimitMS)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log-level must be debug/info/warn/error, got %q", c.LogLevel)
	}
	if c.ScanTimeout <= 0 {
		return fmt.Errorf("scan-timeout must be positive, got %v", c.ScanTimeout)
	}
	for k, val := range c.RegistryMap {
		if k == "" || val == "" {
			return fmt.Errorf("registry-map entry has empty key or value")
		}
		if strings.HasPrefix(val, "/") {
			return fmt.Errorf("registry-map value %q must not start with /", val)
		}
	}
	return nil
}

// SlogLevel returns the slog.Level matching c.LogLevel. Quiet mode raises the
// effective level to WARN so INFO lines are dropped.
func (c *CLIConfig) SlogLevel() slog.Level {
	if c.Quiet {
		return slog.LevelWarn
	}
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

// resolveRegistryMap normalizes the registry map from either a map (flag,
// config file) or a "k=v,k=v" string (env var) into a map[string]string.
// If both sources contributed, Viper's precedence already picked the winner;
// this function only handles the type coercion.
func resolveRegistryMap(v *viper.Viper) (map[string]string, error) {
	raw := v.Get("registry-map")
	if raw == nil {
		return nil, nil
	}

	switch x := raw.(type) {
	case map[string]string:
		return copyMap(x), nil
	case map[string]any:
		out := make(map[string]string, len(x))
		for k, val := range x {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("registry-map value for %q is not a string: %T", k, val)
			}
			out[k] = s
		}
		return out, nil
	case string:
		return parseRegistryMapString(x)
	default:
		return nil, fmt.Errorf("registry-map has unexpected type %T", raw)
	}
}

// parseRegistryMapString parses "key=value,key=value" from an env var.
func parseRegistryMapString(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, raw := range strings.Split(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("registry-map entry %q must be key=value", entry)
		}
		key := strings.TrimSpace(entry[:eq])
		val := strings.TrimSpace(entry[eq+1:])
		if key == "" || val == "" {
			return nil, fmt.Errorf("registry-map entry %q has empty key or value", entry)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("registry-map entry %q duplicates key %q", entry, key)
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// xdgConfigHome returns $XDG_CONFIG_HOME, falling back to $HOME/.config.
func xdgConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config")
	}
	return ""
}
