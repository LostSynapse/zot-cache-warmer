// Package cli holds defaults and helpers shared between the cluster CronJob
// (cmd/zot-cache-warmer) and the standalone CLI (cmd/zot-warm). Both binaries
// MUST read defaults from here rather than duplicating values, so a change
// to the canonical registry map ships uniformly across both surfaces.
package cli

import "time"

// DefaultRegistryMap is the upstream-registry → Zot-local-path-prefix mapping
// baked into both binaries. Aligns with the `destination` values in the Zot
// sync.registries config and the endpoint paths in the k3s registries.yaml
// for the lost-synapse cluster. Operators in different environments override
// this via the ZOT_REGISTRY_MAP env var or a config file.
//
// Adding a new upstream is a three-file change (warmer config, Zot ConfigMap,
// k3s registries.yaml on every node) — see the operator guide.
var DefaultRegistryMap = map[string]string{
	"docker.io":       "docker-images",
	"ghcr.io":         "ghcr-images",
	"registry.k8s.io": "k8s-images",
	"quay.io":         "quay-images",
	"gcr.io":          "gcr-images",
	"code.forgejo.org": "forgejo-images",
	"lscr.io":         "lscr-images",
}

// DefaultZotURL is the in-cluster Zot service. Both binaries override via
// configuration; this default exists so the standalone CLI runs with no
// flags in environments that match the lost-synapse layout.
const DefaultZotURL = "https://zot.lost-synapse.com"

// DefaultRateLimitMS is the inter-request delay in milliseconds between
// sequential Zot manifest requests. Sequential warming is intentional;
// concurrent warming amplifies upstream rate-limit pressure.
const DefaultRateLimitMS = 250

// DefaultLogLevel is the slog level shipped to stdout/stderr.
const DefaultLogLevel = "info"

// DefaultScanTimeout is the overall context deadline for a single run. First-
// run cache warming is dominated by upstream pull-through latency.
const DefaultScanTimeout = 15 * time.Minute

// DefaultProbeTimeout is the per-image HEAD probe timeout.
const DefaultProbeTimeout = 30 * time.Second

// DefaultWarmTimeout is the per-image GET warm timeout. Generous because
// blob-inclusive pull-through of large images can take minutes.
const DefaultWarmTimeout = 10 * time.Minute
