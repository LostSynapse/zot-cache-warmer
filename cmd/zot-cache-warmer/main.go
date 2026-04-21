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
//  4. Hand the discovered image list to the shared processor package, which
//     parses, probes, and warms each one.
//  5. Emit a structured summary to stdout and exit.
//
// The process is idempotent and stateless. Per-image failures are logged and
// counted, never fatal — the cluster CronJob's contract is "best effort"
// because the next scheduled run will retry naturally. Use cmd/zot-warm
// instead if you need strict pre-deploy gating semantics.
package main

import (
	_ "crypto/sha256" // required: registers sha256 for opencontainers/go-digest
	_ "crypto/sha512" // optional: accept sha512-pinned references

	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lostsynapse/zot-cache-warmer/internal/config"
	"github.com/lostsynapse/zot-cache-warmer/internal/kube"
	"github.com/lostsynapse/zot-cache-warmer/internal/processor"
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
	userAgent := fmt.Sprintf("zot-cache-warmer/%s", Version)
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

	// --- Process images via shared core ---
	s, err := processor.Process(ctx, processor.Options{
		ZotURL:      cfg.ZotRegistryURL,
		ZotUsername: cfg.ZotUsername,
		ZotPassword: cfg.ZotPassword,
		ZotInsecure: cfg.ZotInsecure,
		RegistryMap: cfg.RegistryMap,
		RateLimit:   time.Duration(cfg.RateLimitMS) * time.Millisecond,
		Version:     Version,
		Logger:      logger,
	}, images)
	if err != nil {
		logger.Error("processor init failed", "error", err.Error())
		return 1
	}

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

	// Cluster CronJob contract: best-effort. Per-image failures don't
	// fail the run — the next scheduled execution retries naturally.
	return 0
}
