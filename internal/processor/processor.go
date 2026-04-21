// Package processor implements the core warming loop: parse references,
// probe Zot for cache presence, GET to trigger pull-through warming, count
// outcomes. It is the only place where the warming algorithm lives — both
// the cluster CronJob and the standalone CLI call into Process().
package processor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"time"

	"github.com/lostsynapse/zot-cache-warmer/internal/cli"
	"github.com/lostsynapse/zot-cache-warmer/internal/parser"
	"github.com/lostsynapse/zot-cache-warmer/internal/registry"
)

// Stats records per-run outcomes. Returned by Process for the caller to log
// and for the CLI to derive its exit code from.
type Stats struct {
	Total            int
	Parsed           int
	ParseErrors      int
	SkippedNoMapping int
	Cached           int
	Warmed           int
	ProbeErrors      int
	WarmErrors       int
}

// AnyHardFailure returns true if any condition that should stop a CI gate
// occurred: parse errors, probe errors that didn't recover, or warm errors.
// "Skipped because no mapping" is intentionally NOT a hard failure — that
// outcome is operator-visible and informational.
func (s Stats) AnyHardFailure() bool {
	return s.ParseErrors > 0 || s.WarmErrors > 0
}

// Options is the input to Process. All fields are required except
// ProbeTimeout and WarmTimeout, which fall back to defaults if zero.
type Options struct {
	// ZotURL is the full URL (scheme://host[:port]) of Zot. Scheme determines
	// HTTP vs HTTPS transport.
	ZotURL string

	// ZotUsername / ZotPassword are HTTP Basic credentials for Zot, if Zot's
	// accessControl requires authenticated reads. Empty username ⇒ anonymous.
	ZotUsername string
	ZotPassword string

	// ZotInsecure skips TLS certificate verification. Self-signed dev only.
	ZotInsecure bool

	// RegistryMap is upstream-registry → Zot-local-path-prefix. When non-
	// empty, images from registries not in the map are skipped (counted in
	// SkippedNoMapping). When empty, bare repository paths are sent (the
	// flatten layout).
	RegistryMap map[string]string

	// RateLimit is the delay between sequential Zot requests.
	RateLimit time.Duration

	// ProbeTimeout per-image HEAD timeout. Defaults to cli.DefaultProbeTimeout.
	ProbeTimeout time.Duration

	// WarmTimeout per-image GET timeout. Defaults to cli.DefaultWarmTimeout.
	WarmTimeout time.Duration

	// Version embedded in the User-Agent header.
	Version string

	// Logger receives all output.
	Logger *slog.Logger
}

// Process runs the warm loop over the supplied images and returns Stats.
// ctx controls the overall budget; per-image timeouts are derived from
// Options.ProbeTimeout and Options.WarmTimeout. Returns an error only for
// initialization failures (bad URL, transport setup); per-image failures are
// counted in Stats and never returned as errors.
func Process(ctx context.Context, opts Options, images []string) (Stats, error) {
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = cli.DefaultProbeTimeout
	}
	if opts.WarmTimeout == 0 {
		opts.WarmTimeout = cli.DefaultWarmTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	zotURL, err := url.Parse(opts.ZotURL)
	if err != nil {
		return Stats{}, fmt.Errorf("parse Zot URL: %w", err)
	}
	if zotURL.Scheme != "http" && zotURL.Scheme != "https" {
		return Stats{}, fmt.Errorf("Zot URL scheme must be http or https, got %q", zotURL.Scheme)
	}
	if zotURL.Host == "" {
		return Stats{}, fmt.Errorf("Zot URL must include a host")
	}

	httpTransport, err := registry.NewTransport(registry.TransportConfig{
		InsecureSkipVerify:  opts.ZotInsecure,
		TLSHandshakeTimeout: 10 * time.Second,
		DialTimeout:         5 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 64,
	})
	if err != nil {
		return Stats{}, fmt.Errorf("http transport init: %w", err)
	}
	httpClient := &http.Client{Transport: httpTransport}

	userAgent := fmt.Sprintf(
		"zot-cache-warmer/%s (go %s; %s/%s)",
		opts.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)

	warmer, err := registry.NewWarmer(
		zotURL.Host,
		httpClient,
		opts.ZotUsername, opts.ZotPassword,
		zotURL.Scheme == "http",
		userAgent,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("registry warmer init: %w", err)
	}

	return processImages(ctx, warmer, images, opts), nil
}

// processImages iterates the image set sequentially, with a configurable
// delay between requests. Sequential (not concurrent) because cache warming
// is a background task and overwhelming Zot or upstream registries is the
// bigger risk than finishing slowly.
func processImages(ctx context.Context, w *registry.Warmer, images []string, opts Options) Stats {
	var s Stats
	s.Total = len(images)
	logger := opts.Logger

	for _, raw := range images {
		if err := ctx.Err(); err != nil {
			logger.Warn("context cancelled, stopping",
				"processed", s.Parsed,
				"remaining", s.Total-s.Parsed-s.ParseErrors,
				"error", err.Error(),
			)
			return s
		}

		p, err := parser.Parse(raw)
		if err != nil {
			s.ParseErrors++
			logger.Warn("parse failure",
				"raw", parser.Sanitize(raw),
				"category", parser.ClassifyError(err),
				"error", err.Error(),
			)
			continue
		}
		s.Parsed++

		// Determine the Zot-local path prefix for this image's registry.
		// If a RegistryMap is configured, use it strictly: images whose
		// registry has no mapping are skipped (their Zot URL would fail
		// because no sync.content.destination matches). If the map is
		// empty, fall back to the bare repository path (flatten layout).
		var zotPath string
		if len(opts.RegistryMap) > 0 {
			dest, ok := opts.RegistryMap[p.Registry]
			if !ok {
				s.SkippedNoMapping++
				logger.Warn("registry not in ZOT_REGISTRY_MAP, skipping",
					"canonical", p.Canonical,
					"registry", p.Registry,
					"hint", "add this registry to ZOT_REGISTRY_MAP, Zot sync.registries, and k3s registries.yaml",
				)
				continue
			}
			zotPath = dest + "/" + p.Repository
		} else {
			zotPath = p.Repository
		}

		var zotRef string
		switch {
		case p.IsDigestOnly:
			zotRef = zotPath + "@" + p.Digest
		default:
			zotRef = zotPath + ":" + p.Tag
		}

		logger.Debug("processing image",
			"canonical", p.Canonical,
			"zot_ref", zotRef,
			"had_tag_and_digest", p.HadBothTagAndDigest,
			"digest_only", p.IsDigestOnly,
		)

		// --- cache-presence probe (HEAD) ---
		probeCtx, probeCancel := context.WithTimeout(ctx, opts.ProbeTimeout)
		_, cached, err := w.IsCached(probeCtx, zotRef)
		probeCancel()
		switch {
		case err != nil:
			// Probe failure is ambiguous — could be Zot doing a synchronous
			// upstream pull-through that outran the probe budget, a network
			// blip, or the registry being genuinely unreachable. None of
			// those mean "skip this image"; fall through to warm, whose
			// budget is large enough for the pull-through latency that Zot
			// exhibits on first cache-miss.
			s.ProbeErrors++
			logger.Warn("cache probe failed, attempting warm anyway",
				"canonical", p.Canonical,
				"zot_ref", zotRef,
				"error", err.Error(),
			)
			// NO continue — fall through to warm.
		case cached:
			s.Cached++
			logger.Debug("cache hit", "canonical", p.Canonical, "zot_ref", zotRef)
			sleep(ctx, opts.RateLimit)
			continue
		}

		// --- warm (GET) ---
		// Either probe said "not cached" (404) or probe errored — in both
		// cases we attempt to warm.
		warmCtx, warmCancel := context.WithTimeout(ctx, opts.WarmTimeout)
		if p.IsDigestOnly {
			_, err = w.Warm(warmCtx, zotRef)
		} else {
			err = w.WarmMultiArch(warmCtx, zotRef)
		}
		warmCancel()

		if err != nil {
			s.WarmErrors++
			logger.Warn("warm failed",
				"canonical", p.Canonical,
				"zot_ref", zotRef,
				"error", err.Error(),
			)
		} else {
			s.Warmed++
			logger.Info("warmed",
				"canonical", p.Canonical,
				"zot_ref", zotRef,
				"had_tag_and_digest", p.HadBothTagAndDigest,
			)
		}

		sleep(ctx, opts.RateLimit)
	}

	return s
}

// sleep waits for d, returning early if ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
		return
	}
}
