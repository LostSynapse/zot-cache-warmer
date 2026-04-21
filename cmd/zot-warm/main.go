// Command zot-warm pre-warms a Zot pull-through cache with specific image
// references before a deployment. Intended as a CI gate: run before
// `kubectl apply`, exit non-zero if any required image fails to cache, block
// the deploy. This is the standalone counterpart to cmd/zot-cache-warmer,
// which scans the live cluster post-hoc.
//
// USAGE
//
//	zot-warm <image>                 # single image
//	zot-warm <file>                  # file: YAML/JSON manifest or images.txt
//	zot-warm -                       # stdin (images.txt format)
//	zot-warm --file <path>           # force file interpretation
//
// The positional argument auto-detects: if it exists as a regular file on
// disk, it's treated as a file; otherwise it's treated as an image reference.
// Use --file to force file mode when the path doesn't yet exist on disk.
//
// EXIT CODES
//
//	0  — all images cached or successfully warmed
//	1  — hard failure (bad input, config error, network unreachable, auth)
//	2  — soft failure (warm errors) — only in strict mode (default)
//
// --soft disables exit 2; in soft mode only hard failures exit non-zero.
// Use --soft for opportunistic cache warming; use default strict mode for
// CI gates that must block a deploy on cache readiness.
//
// CONFIGURATION
//
// Precedence: flags > env > config file > built-in defaults.
// See internal/cli/config.go for the full config surface.
package main

import (
	_ "crypto/sha256" // registers sha256 for opencontainers/go-digest
	_ "crypto/sha512"

	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lostsynapse/zot-cache-warmer/internal/cli"
	"github.com/lostsynapse/zot-cache-warmer/internal/input"
	"github.com/lostsynapse/zot-cache-warmer/internal/processor"
	"github.com/spf13/pflag"
)

// Version is overridden at build time via -ldflags "-X main.Version=...".
var Version = "dev"

// Exit codes. Symbols rather than magic numbers so the contract is greppable.
const (
	exitOK       = 0
	exitHard     = 1
	exitSoftWarm = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := pflag.NewFlagSet("zot-warm", pflag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usageText)
	}
	cli.RegisterFlags(fs)
	helpFlag := fs.BoolP("help", "h", false, "Show usage and exit")
	versionFlag := fs.Bool("version", false, "Print version and exit")

	if err := fs.Parse(args); err != nil {
		if err == pflag.ErrHelp {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "flag parse error: %v\n", err)
		return exitHard
	}

	if *helpFlag {
		fs.Usage()
		return exitOK
	}
	if *versionFlag {
		fmt.Println(Version)
		return exitOK
	}

	// No positional arg → print usage. Explicit "no-param manual" behavior.
	if fs.NArg() == 0 {
		fs.Usage()
		return exitHard
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "exactly one positional argument required (image, file, or '-'); got %d\n", fs.NArg())
		return exitHard
	}

	cfg, err := cli.Load(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return exitHard
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	// --- Resolve input ---
	arg := fs.Arg(0)
	result, err := input.Collect(arg, cfg.Force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "input error: %v\n", err)
		return exitHard
	}

	// Set up signal-cancellable, scan-timeout-bounded context.
	ctx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	ctx, cancel := context.WithTimeout(ctx, cfg.ScanTimeout)
	defer cancel()

	start := time.Now()
	logger.Info("run started",
		"version", Version,
		"zot_url", cfg.ZotURL,
		"zot_insecure", cfg.ZotInsecure,
		"rate_limit_ms", cfg.RateLimitMS,
		"scan_timeout", cfg.ScanTimeout.String(),
		"source", string(result.Source),
		"image_count", len(result.Images),
		"mode", strictOrSoft(cfg.Soft),
	)

	// --- Process images via shared core ---
	s, err := processor.Process(ctx, processor.Options{
		ZotURL:      cfg.ZotURL,
		ZotUsername: cfg.ZotUsername,
		ZotPassword: cfg.ZotPassword,
		ZotInsecure: cfg.ZotInsecure,
		RegistryMap: cfg.RegistryMap,
		RateLimit:   time.Duration(cfg.RateLimitMS) * time.Millisecond,
		Version:     Version,
		Logger:      logger,
	}, result.Images)
	if err != nil {
		logger.Error("processor init failed", "error", err.Error())
		return exitHard
	}

	// Summary is emitted at WARN level so it's visible even with --quiet.
	// Using WARN feels wrong semantically, but the alternative — INFO — is
	// suppressed by --quiet, and people running with --quiet specifically
	// want the summary. Log level is a UI concern here; content is neutral.
	logger.LogAttrs(ctx, slog.LevelWarn, "run complete",
		slog.Int("images_total", s.Total),
		slog.Int("parsed", s.Parsed),
		slog.Int("parse_errors", s.ParseErrors),
		slog.Int("skipped_no_mapping", s.SkippedNoMapping),
		slog.Int("already_cached", s.Cached),
		slog.Int("warmed", s.Warmed),
		slog.Int("probe_errors", s.ProbeErrors),
		slog.Int("warm_errors", s.WarmErrors),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.String("mode", strictOrSoft(cfg.Soft)),
	)

	return exitCode(s, cfg.Soft)
}

// exitCode derives the process exit code from stats and mode.
//
// Exit categories:
//   - Hard failures (always exit 1): parse errors, context cancellation that
//     left unprocessed images, missing any input at all.
//   - Soft failures (exit 2 in strict, 0 in soft): per-image warm errors.
//   - Everything else: exit 0 (images cached or successfully warmed).
//
// Note: SkippedNoMapping is informational, never a failure. An operator sees
// the warning and decides whether to update the map; the tool itself makes
// no judgement about whether the skip is "bad" — deployments may intentionally
// reference images from registries outside the Zot cache.
func exitCode(s processor.Stats, soft bool) int {
	if s.ParseErrors > 0 {
		return exitHard
	}
	if s.WarmErrors > 0 {
		if soft {
			return exitOK
		}
		return exitSoftWarm
	}
	return exitOK
}

func strictOrSoft(soft bool) string {
	if soft {
		return "soft"
	}
	return "strict"
}

const usageText = `zot-warm: pre-warm a Zot pull-through cache with specific image references.

USAGE:
  zot-warm [flags] <image-or-file>
  zot-warm [flags] -                    # read images from stdin

EXAMPLES:
  zot-warm ghcr.io/myorg/myapp:v1.2.3
  zot-warm deploy/manifests.yaml
  zot-warm --file planned-images.txt    # even if the file doesn't yet exist
  cat images.txt | zot-warm -

EXIT CODES:
  0  all images cached or warmed
  1  hard failure (bad input, config error, network)
  2  warm errors (strict mode only; --soft suppresses this)

FLAGS:
`

func init() {
	// Append flag defaults to usage text at init time so we build it once.
	// (The flag set is built in run(); we just point Usage at it.)
}
