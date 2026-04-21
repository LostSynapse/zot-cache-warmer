package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

// newTestFlagSet returns a freshly-registered flag set for each test so state
// doesn't leak between parallel cases.
func newTestFlagSet(t *testing.T) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	return fs
}

// withEnv sets the provided env vars for the duration of the test and clears
// every known zot-warm-related variable beforehand to isolate each case.
func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	knowns := []string{
		"ZOT_WARM_ZOT_URL", "ZOT_WARM_ZOT_USERNAME", "ZOT_WARM_ZOT_PASSWORD",
		"ZOT_WARM_ZOT_INSECURE", "ZOT_WARM_REGISTRY_MAP", "ZOT_WARM_RATE_LIMIT_MS",
		"ZOT_WARM_LOG_LEVEL", "ZOT_WARM_SCAN_TIMEOUT", "ZOT_WARM_QUIET",
		"ZOT_WARM_SOFT", "ZOT_WARM_FILE",
		"ZOT_REGISTRY_URL", "ZOT_USERNAME", "ZOT_PASSWORD", "ZOT_REGISTRY_MAP",
		"XDG_CONFIG_HOME", "HOME",
	}
	for _, k := range knowns {
		t.Setenv(k, "")
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	// Redirect XDG/HOME to a scratch dir so user-level config files don't
	// accidentally affect test outcomes.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestLoad_Defaults(t *testing.T) {
	withEnv(t, nil)
	fs := newTestFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ZotURL != DefaultZotURL {
		t.Errorf("ZotURL = %q, want %q", c.ZotURL, DefaultZotURL)
	}
	if c.LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", c.LogLevel, DefaultLogLevel)
	}
	if c.ScanTimeout != DefaultScanTimeout {
		t.Errorf("ScanTimeout = %v, want %v", c.ScanTimeout, DefaultScanTimeout)
	}
	if c.RateLimitMS != DefaultRateLimitMS {
		t.Errorf("RateLimitMS = %d, want %d", c.RateLimitMS, DefaultRateLimitMS)
	}
	if c.Quiet {
		t.Error("default Quiet should be false")
	}
	if c.Soft {
		t.Error("default Soft should be false")
	}
	if c.Force {
		t.Error("default Force should be false")
	}
	if !reflect.DeepEqual(c.RegistryMap, DefaultRegistryMap) {
		t.Errorf("RegistryMap = %v, want %v", c.RegistryMap, DefaultRegistryMap)
	}
}

func TestLoad_FlagsOverrideDefaults(t *testing.T) {
	withEnv(t, nil)
	fs := newTestFlagSet(t)
	args := []string{
		"--zot-url=http://localhost:5000",
		"--log-level=debug",
		"--scan-timeout=1m",
		"--quiet",
		"--soft",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ZotURL != "http://localhost:5000" {
		t.Errorf("ZotURL = %q", c.ZotURL)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if c.ScanTimeout != time.Minute {
		t.Errorf("ScanTimeout = %v", c.ScanTimeout)
	}
	if !c.Quiet {
		t.Error("Quiet should be true")
	}
	if !c.Soft {
		t.Error("Soft should be true")
	}
}

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	withEnv(t, map[string]string{
		"ZOT_WARM_ZOT_URL":      "http://from-env:5000",
		"ZOT_WARM_LOG_LEVEL":    "warn",
		"ZOT_WARM_RATE_LIMIT_MS": "500",
	})
	fs := newTestFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ZotURL != "http://from-env:5000" {
		t.Errorf("ZotURL = %q, want env value", c.ZotURL)
	}
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn", c.LogLevel)
	}
	if c.RateLimitMS != 500 {
		t.Errorf("RateLimitMS = %d, want 500", c.RateLimitMS)
	}
}

func TestLoad_FlagBeatsEnv(t *testing.T) {
	withEnv(t, map[string]string{
		"ZOT_WARM_ZOT_URL": "http://from-env:5000",
	})
	fs := newTestFlagSet(t)
	if err := fs.Parse([]string{"--zot-url=http://from-flag:5000"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ZotURL != "http://from-flag:5000" {
		t.Errorf("flag should override env: got %q", c.ZotURL)
	}
}

func TestLoad_CompatibilityEnvAliases(t *testing.T) {
	// ZOT_REGISTRY_URL (no ZOT_WARM_ prefix) should still be honored for
	// cluster-CronJob Secret compatibility.
	withEnv(t, map[string]string{
		"ZOT_REGISTRY_URL": "http://compat-env:5000",
		"ZOT_USERNAME":     "user",
		"ZOT_PASSWORD":     "pass",
		"ZOT_REGISTRY_MAP": "docker.io=docker-images,ghcr.io=ghcr-images",
	})
	fs := newTestFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ZotURL != "http://compat-env:5000" {
		t.Errorf("ZotURL = %q, want compat env value", c.ZotURL)
	}
	if c.ZotUsername != "user" {
		t.Errorf("ZotUsername = %q, want user", c.ZotUsername)
	}
	if c.ZotPassword != "pass" {
		t.Errorf("ZotPassword = %q, want pass", c.ZotPassword)
	}
	want := map[string]string{"docker.io": "docker-images", "ghcr.io": "ghcr-images"}
	if !reflect.DeepEqual(c.RegistryMap, want) {
		t.Errorf("RegistryMap = %v, want %v", c.RegistryMap, want)
	}
}

func TestLoad_ConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zot-warm.yaml")
	content := `zot-url: https://zot.example.com
log-level: debug
rate-limit-ms: 100
scan-timeout: 2m
registry-map:
  docker.io: docker-images
  ghcr.io: ghcr-images
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	withEnv(t, nil)
	// findConfigFile walks CWD first; chdir into dir so ./zot-warm.yaml is found.
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	fs := newTestFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ZotURL != "https://zot.example.com" {
		t.Errorf("ZotURL from file = %q", c.ZotURL)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel from file = %q", c.LogLevel)
	}
	if c.ScanTimeout != 2*time.Minute {
		t.Errorf("ScanTimeout from file = %v", c.ScanTimeout)
	}
	if !reflect.DeepEqual(c.RegistryMap, map[string]string{"docker.io": "docker-images", "ghcr.io": "ghcr-images"}) {
		t.Errorf("RegistryMap from file = %v", c.RegistryMap)
	}
}

func TestLoad_ValidateErrors(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		env   map[string]string
		want  string // substring that must appear in the error
	}{
		{"bad-scheme", []string{"--zot-url=ftp://foo"}, nil, "zot-url must start with"},
		{"bad-log-level", []string{"--log-level=trace"}, nil, "log-level must be"},
		{"negative-rate", []string{"--rate-limit-ms=-1"}, nil, "rate-limit-ms"},
		{"empty-url-via-env", nil, map[string]string{"ZOT_WARM_ZOT_URL": ""}, "zot-url is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Start with default URL blanked so the test can exercise "missing URL".
			env := map[string]string{"ZOT_WARM_ZOT_URL": ""}
			for k, v := range tc.env {
				env[k] = v
			}
			withEnv(t, env)
			// If the test expects a validation error for empty URL, also wipe
			// the default by passing --zot-url=""; otherwise, flag parsing
			// would fall back to the compiled-in default.
			args := tc.flags
			if tc.name == "empty-url-via-env" {
				args = append([]string{"--zot-url="}, args...)
			}
			fs := newTestFlagSet(t)
			if err := fs.Parse(args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err := Load(fs)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseRegistryMapString(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace-only", "   ", nil, false},
		{"single", "docker.io=docker-images", map[string]string{"docker.io": "docker-images"}, false},
		{"multi-with-whitespace", " docker.io=docker-images , ghcr.io=ghcr-images ", map[string]string{"docker.io": "docker-images", "ghcr.io": "ghcr-images"}, false},
		{"missing-equals", "docker.io docker-images", nil, true},
		{"empty-key", "=docker-images", nil, true},
		{"empty-value", "docker.io=", nil, true},
		{"duplicate-key", "docker.io=a,docker.io=b", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRegistryMapString(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSlogLevel_QuietOverridesLogLevel(t *testing.T) {
	c := &CLIConfig{LogLevel: "debug", Quiet: true}
	if c.SlogLevel().String() != "WARN" {
		t.Errorf("Quiet should force WARN level, got %v", c.SlogLevel())
	}
	c.Quiet = false
	if c.SlogLevel().String() != "DEBUG" {
		t.Errorf("without Quiet should reflect LogLevel, got %v", c.SlogLevel())
	}
}
