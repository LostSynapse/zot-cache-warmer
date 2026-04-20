package config

import (
	"log/slog"
	"reflect"
	"testing"
	"time"
)

// withEnv temporarily sets env vars for the duration of the test.
// Any variable set to "" is unset instead of set to empty-string.
func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	// The test process inherits the ambient environment; start by clearing
	// every variable we touch so missing entries in `vars` mean "unset".
	knowns := []string{
		"ZOT_REGISTRY_URL", "ZOT_USERNAME", "ZOT_PASSWORD", "ZOT_INSECURE",
		"RATE_LIMIT_MS", "LOG_LEVEL", "SCAN_TIMEOUT",
		"NAMESPACE_INCLUDE", "NAMESPACE_EXCLUDE",
		"ZOT_REGISTRY_MAP",
	}
	for _, k := range knowns {
		t.Setenv(k, "")
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func TestFromEnv_MinimalValid(t *testing.T) {
	withEnv(t, map[string]string{
		"ZOT_REGISTRY_URL": "http://zot.example.com:5000",
	})
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ZotRegistryURL != "http://zot.example.com:5000" {
		t.Errorf("ZotRegistryURL = %q", c.ZotRegistryURL)
	}
	if c.RateLimitMS != 250 {
		t.Errorf("default RateLimitMS = %d, want 250", c.RateLimitMS)
	}
	if c.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", c.LogLevel)
	}
	if c.ScanTimeout != 15*time.Minute {
		t.Errorf("default ScanTimeout = %v, want 15m", c.ScanTimeout)
	}
	if c.ZotInsecure {
		t.Error("default ZotInsecure should be false")
	}
	if len(c.NamespaceInclude) != 0 || len(c.NamespaceExclude) != 0 {
		t.Errorf("default namespace filters should be empty")
	}
}

func TestFromEnv_MissingURL(t *testing.T) {
	withEnv(t, nil)
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when ZOT_REGISTRY_URL unset")
	}
}

func TestFromEnv_InvalidURL(t *testing.T) {
	tests := []string{
		"ftp://zot.example.com",        // unsupported scheme
		"://broken",                    // no scheme
		"http://",                      // no host
	}
	for _, url := range tests {
		t.Run(url, func(t *testing.T) {
			withEnv(t, map[string]string{"ZOT_REGISTRY_URL": url})
			if _, err := FromEnv(); err == nil {
				t.Errorf("expected error for url %q", url)
			}
		})
	}
}

func TestFromEnv_AllValuesSet(t *testing.T) {
	withEnv(t, map[string]string{
		"ZOT_REGISTRY_URL":   "https://zot.example.com:5000",
		"ZOT_USERNAME":       "warmer",
		"ZOT_PASSWORD":       "secret",
		"ZOT_INSECURE":       "true",
		"RATE_LIMIT_MS":      "500",
		"LOG_LEVEL":          "debug",
		"SCAN_TIMEOUT":       "10m",
		"NAMESPACE_INCLUDE":  "prod, staging ,  ",
		"NAMESPACE_EXCLUDE":  "test",
	})
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ZotUsername != "warmer" || c.ZotPassword != "secret" {
		t.Errorf("credentials not propagated: %q / %q", c.ZotUsername, c.ZotPassword)
	}
	if !c.ZotInsecure {
		t.Error("ZOT_INSECURE=true not applied")
	}
	if c.RateLimitMS != 500 {
		t.Errorf("RateLimitMS = %d, want 500", c.RateLimitMS)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", c.LogLevel)
	}
	if c.ScanTimeout != 10*time.Minute {
		t.Errorf("ScanTimeout = %v, want 10m", c.ScanTimeout)
	}
	wantInc := []string{"prod", "staging"}
	if !reflect.DeepEqual(c.NamespaceInclude, wantInc) {
		t.Errorf("NamespaceInclude = %v, want %v", c.NamespaceInclude, wantInc)
	}
	wantExc := []string{"test"}
	if !reflect.DeepEqual(c.NamespaceExclude, wantExc) {
		t.Errorf("NamespaceExclude = %v, want %v", c.NamespaceExclude, wantExc)
	}
}

func TestFromEnv_InvalidValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"bad-bool", map[string]string{"ZOT_REGISTRY_URL": "http://z", "ZOT_INSECURE": "maybe"}},
		{"bad-int", map[string]string{"ZOT_REGISTRY_URL": "http://z", "RATE_LIMIT_MS": "lots"}},
		{"negative-int", map[string]string{"ZOT_REGISTRY_URL": "http://z", "RATE_LIMIT_MS": "-1"}},
		{"bad-level", map[string]string{"ZOT_REGISTRY_URL": "http://z", "LOG_LEVEL": "verbose"}},
		{"bad-duration", map[string]string{"ZOT_REGISTRY_URL": "http://z", "SCAN_TIMEOUT": "forever"}},
		{"zero-duration", map[string]string{"ZOT_REGISTRY_URL": "http://z", "SCAN_TIMEOUT": "0s"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			if _, err := FromEnv(); err == nil {
				t.Errorf("expected error for %v", tc.env)
			}
		})
	}
}

func TestFromEnv_RegistryMap(t *testing.T) {
	withEnv(t, map[string]string{
		"ZOT_REGISTRY_URL":  "http://z",
		"ZOT_REGISTRY_MAP": "docker.io=docker-images, ghcr.io=ghcr-images ,registry.k8s.io=k8s-images",
	})
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	want := map[string]string{
		"docker.io":       "docker-images",
		"ghcr.io":         "ghcr-images",
		"registry.k8s.io": "k8s-images",
	}
	if !reflect.DeepEqual(c.RegistryMap, want) {
		t.Errorf("RegistryMap = %v, want %v", c.RegistryMap, want)
	}
}

func TestFromEnv_RegistryMapEmpty(t *testing.T) {
	withEnv(t, map[string]string{"ZOT_REGISTRY_URL": "http://z"})
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.RegistryMap != nil {
		t.Errorf("expected nil RegistryMap, got %v", c.RegistryMap)
	}
}

func TestParseRegistryMap_Invalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no-equals", "docker.io docker-images"},
		{"empty-value", "docker.io="},
		{"empty-key", "=docker-images"},
		{"leading-slash-value", "docker.io=/docker-images"},
		{"duplicate-key", "docker.io=a,docker.io=b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRegistryMap(tc.in); err == nil {
				t.Errorf("expected error for %q", tc.in)
			}
		})
	}
}

func TestParseRegistryMap_WhitespaceAndEmptyEntries(t *testing.T) {
	got, err := parseRegistryMap("  docker.io = docker-images ,, ghcr.io=ghcr-images ,")
	if err != nil {
		t.Fatalf("parseRegistryMap: %v", err)
	}
	want := map[string]string{
		"docker.io": "docker-images",
		"ghcr.io":   "ghcr-images",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo},
	}
	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			c := &Config{LogLevel: tc.level}
			if got := c.SlogLevel(); got != tc.want {
				t.Errorf("SlogLevel() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,  , c ", []string{"a", "b", "c"}},
		{",,", nil},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := splitCSV(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
