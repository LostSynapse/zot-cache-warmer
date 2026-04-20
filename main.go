// Command zot-cache-warmer enumerates every container image reference in use
// across a Kubernetes cluster and ensures each one is present in a Zot
// pull-through cache. It is designed to run as a Kubernetes CronJob.
//
// Workflow:
//  1. Load configuration from environment variables.
//  2. Authenticate to the Kubernetes API via the pod's projected service-
//     account token.
//  3. List Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, and
//     CronJobs across the cluster; extract every init/main/ephemeral container
//     image reference; deduplicate.
//  4. For each reference: parse and normalize; HEAD Zot to probe cache
//     presence; if missing, GET to trigger Zot's pull-through sync.
//  5. Emit a structured summary to stdout and exit.
//
// The process is idempotent and stateless. Failures are logged and skipped,
// never fatal, on the theory that the next scheduled run will retry naturally.
package main

import (
	_ "crypto/sha256" // required: registers sha256 for opencontainers/go-digest
	_ "crypto/sha512" // optional: accept sha512-pinned references

	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/lostsynapse/zot-cache-warmer/internal/config"
	"github.com/lostsynapse/zot-cache-warmer/internal/kube"
	"github.com/lostsynapse/zot-cache-warmer/internal/parser"
	"github.com/lostsynapse/zot-cache-warmer/internal/registry"
)

// Version is overridden at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	userAgent := fmt.Sprintf(
		"zot-cache-warmer/%s (go %s; %s/%s)",
		Version, runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)

	ctx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	ctx, cancel := context.WithTimeout(ctx, cfg.ScanTimeout)
	defer cancel()

	start := time.Now()
	logger.Info("run started",
		"version", Version,
		"zot_url", cfg.ZotRegistryURL,
		"zot_insecure", cfg.ZotInsecure,
		"rate_limit_ms", cfg.RateLimitMS,
		"scan_timeout", cfg.ScanTimeout.String(),
		"namespace_include", cfg.NamespaceInclude,
		"namespace_exclude", cfg.NamespaceExclude,
	)

	// --- Kubernetes client ---
	kubeClient, err := kube.NewInClusterClient(userAgent)
	if err != nil {
		logger.Error("kubernetes client init failed", "error", err.Error())
		return 1
	}

	// --- Collect images ---
	collectStart := time.Now()
	images, collectErrs := kube.CollectImages(ctx, kubeClient, kube.Filter{
		Include: cfg.NamespaceInclude,
		Exclude: cfg.NamespaceExclude,
	})
	for _, e := range collectErrs {
		logger.Warn("workload list partial failure", "error", e.Error())
	}
	logger.Info("cluster scan complete",
		"images_found", len(images),
		"list_errors", len(collectErrs),
		"duration_ms", time.Since(collectStart).Milliseconds(),
	)

	if len(images) == 0 {
		logger.Info("no images discovered; nothing to warm",
			"duration_ms", time.Since(start).Milliseconds())
		// Exit 0 — an empty cluster is not a failure.
		return 0
	}

	// --- Build Zot client ---
	zotURL, err := url.Parse(cfg.ZotRegistryURL)
	if err != nil {
		logger.Error("parse ZOT_REGISTRY_URL", "error", err.Error())
		return 1
	}

	httpTransport, err := registry.NewTransport(registry.TransportConfig{
		InsecureSkipVerify:  cfg.ZotInsecure,
		TLSHandshakeTimeout: 10 * time.Second,
		DialTimeout:         5 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 64,
	})
	if err != nil {
		logger.Error("http transport init failed", "error", err.Error())
		return 1
	}
	httpClient := &http.Client{Transport: httpTransport}

	warmer, err := registry.NewWarmer(
		zotURL.Host,
		httpClient,
		cfg.ZotUsername, cfg.ZotPassword,
		zotURL.Scheme == "http",
		userAgent,
	)
	if err != nil {
		logger.Error("registry warmer init failed", "error", err.Error())
		return 1
	}

	// --- Process each image ---
	s := processImages(
		ctx,
		warmer,
		images,
		time.Duration(cfg.RateLimitMS)*time.Millisecond,
		cfg.RegistryMap,
		logger,
	)

	logger.Info("run complete",
		"images_total", s.Total,
		"parsed", s.Parsed,
		"parse_errors", s.ParseErrors,
		"skipped_no_mapping", s.SkippedNoMapping,
		"already_cached", s.Cached,
		"warmed", s.Warmed,
		"probe_errors", s.ProbeErrors,
		"warm_errors", s.WarmErrors,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	return 0
}

// stats tracks per-run outcomes for the final summary log line.
type stats struct {
	Total            int
	Parsed           int
	ParseErrors      int
	SkippedNoMapping int
	Cached           int
	Warmed           int
	ProbeErrors      int
	WarmErrors       int
}

// processImages iterates the discovered image set sequentially, with a
// configurable delay between requests. Sequential (not concurrent) because
// cache warming is a background task and overwhelming Zot or upstream
// registries is the bigger risk than finishing slowly.
//
// When registryMap is non-empty, images whose registry is not a key in the
// map are skipped (tracked in s.SkippedNoMapping) rather than sent to Zot
// on paths that would fail. When registryMap is empty, all images are sent
// to Zot at their bare repository path (the "flatten" layout).
func processImages(
	ctx context.Context,
	w *registry.Warmer,
	images []string,
	delay time.Duration,
	registryMap map[string]string,
	logger *slog.Logger,
) stats {
	var s stats
	s.Total = len(images)

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
		if len(registryMap) > 0 {
			dest, ok := registryMap[p.Registry]
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
		probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
		_, cached, err := w.IsCached(probeCtx, zotRef)
		probeCancel()
		switch {
		case err != nil:
			// Probe failure is ambiguous — could be Zot doing a synchronous
			// upstream pull-through that outran the 30s budget, a network
			// blip, or the registry being genuinely unreachable. None of
			// those mean "skip this image"; fall through to warm, whose
			// 10-minute context has enough runway for the pull-through
			// latency that Zot exhibits on first cache-miss.
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
			sleep(ctx, delay)
			continue
		}

		// --- warm (GET) ---
		// Either probe said "not cached" (404) or probe errored — in both
		// cases we attempt to warm. Blob-inclusive pull-through can take
		// minutes on large images.
		warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Minute)
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

		sleep(ctx, delay)
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
