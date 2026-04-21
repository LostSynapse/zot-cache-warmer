# Zot Cache Warmer

Two complementary tools that keep a Zot pull-through registry primed:

**`zot-cache-warmer`** — Kubernetes CronJob. Scans the cluster periodically, discovers every image in use across Pods / Deployments / StatefulSets / DaemonSets / ReplicaSets / Jobs / CronJobs, and warms each one in Zot. Best-effort; never fails the run.

**`zot-warm`** — standalone CLI. Takes a single image or a manifest file, warms the referenced images, exits non-zero if any fail. Designed as a **CI gate** — run before `kubectl apply`, block the deploy if the cache isn't ready. Also useful for ISP outage resilience and the Zot #2584 `tag@digest` pull-through bug via a shared parser.

Both binaries share the warming core (`internal/parser`, `internal/processor`, `internal/registry`), so a fix in one ships in both.

---

## Quick start — cluster CronJob

```bash
kubectl apply -f deploy/rbac.yaml -f deploy/cronjob.yaml
```

Edit `deploy/cronjob.yaml` and replace every `# CHANGEME:` value first — see inline comments and `deploy/example-values.yaml` for common scenarios. Scheduled runs warm every image in the cluster on a configurable cadence.

## Quick start — standalone CLI

**Install from GitHub Releases:**

```bash
# Single-arch (pick one)
curl -LO https://github.com/lostsynapse/zot-cache-warmer/releases/latest/download/zot-warm-linux-amd64
chmod +x zot-warm-linux-amd64
sudo mv zot-warm-linux-amd64 /usr/local/bin/zot-warm

# Universal (both arches + selector script; recommended for shared install scripts)
curl -LO https://github.com/lostsynapse/zot-cache-warmer/releases/latest/download/zot-warm-linux-universal.tar.gz
tar xzf zot-warm-linux-universal.tar.gz
sudo ./zot-warm/install.sh
```

**Use it:**

```bash
# Warm a single image
zot-warm ghcr.io/myorg/myapp:v1.2.3

# Warm every image referenced in a manifest
zot-warm deploy/app.yaml

# Read images from stdin (one per line)
kubectl get deploy -o json | jq -r '.items[].spec.template.spec.containers[].image' | zot-warm -

# Explicit file (even if path doesn't yet exist on disk)
zot-warm --file planned.yaml

# Opportunistic mode — don't fail the script on warm errors
zot-warm --soft some-chart-output.yaml
```

---

## CI gate example

Block a deploy when the Zot cache can't be primed with required images. Default (strict) mode exits `2` on any warm failure; exit `1` on hard errors (bad input, network, auth).

```yaml
# .github/workflows/deploy.yaml
jobs:
  deploy:
    steps:
      - uses: actions/checkout@v6

      - name: Install zot-warm
        run: |
          curl -sSL -o /tmp/zot-warm \
            https://github.com/lostsynapse/zot-cache-warmer/releases/latest/download/zot-warm-linux-amd64
          chmod +x /tmp/zot-warm

      - name: Warm Zot for planned deploy
        env:
          ZOT_REGISTRY_URL: ${{ secrets.ZOT_URL }}
          ZOT_USERNAME:     ${{ secrets.ZOT_USERNAME }}
          ZOT_PASSWORD:     ${{ secrets.ZOT_PASSWORD }}
        # Strict mode (default): non-zero exit blocks the deploy step
        run: /tmp/zot-warm deploy/rendered-manifest.yaml

      - name: Apply
        run: kubectl apply -f deploy/rendered-manifest.yaml
```

Exit codes:

| Code | Meaning |
|---|---|
| 0 | All images cached or successfully warmed |
| 1 | Hard failure (bad input, config error, network unreachable, auth rejected) |
| 2 | Warm failure — strict mode only (suppressed by `--soft`) |

---

## Configuration

Precedence (highest to lowest):

1. Command-line flags
2. Environment variables (`ZOT_WARM_<UPPER>`, e.g. `ZOT_WARM_ZOT_URL`)
3. Config file: first of `./zot-warm.yaml`, `$XDG_CONFIG_HOME/zot-warm/config.yaml`, `/etc/zot-warm/config.yaml`
4. Built-in defaults (seven-registry map for the lost-synapse cluster)

**Compatibility env aliases:** `ZOT_REGISTRY_URL`, `ZOT_USERNAME`, `ZOT_PASSWORD`, `ZOT_REGISTRY_MAP` are also honored — the same Kubernetes Secret used by the cluster CronJob works for the CLI.

**Config file example:**

```yaml
# zot-warm.yaml
zot-url: https://zot.lost-synapse.com
log-level: info
rate-limit-ms: 250
scan-timeout: 15m
registry-map:
  docker.io: docker-images
  ghcr.io: ghcr-images
  registry.k8s.io: k8s-images
  quay.io: quay-images
  gcr.io: gcr-images
  code.forgejo.org: forgejo-images
  lscr.io: lscr-images
```

See `zot-warm --help` for the full flag surface.

---

## Architecture

Both binaries share a common layout:

```
zot-cache-warmer/
├── cmd/
│   ├── zot-cache-warmer/   # cluster CronJob binary
│   └── zot-warm/           # standalone CLI binary
├── internal/
│   ├── cli/                # shared defaults + Viper config (CLI only)
│   ├── config/             # env-only config (cluster binary only)
│   ├── input/              # image / file / stdin auto-detection (CLI only)
│   ├── kube/               # Kubernetes API client (cluster binary only)
│   ├── parser/             # image reference parsing (shared)
│   ├── processor/          # warming loop — the hot path (shared)
│   └── registry/           # Zot HTTP client (shared)
├── deploy/                 # Kubernetes manifests for the CronJob
├── scripts/                # arch-selector + installer for the universal archive
└── .github/workflows/      # CI: build container image, release binaries
```

Changes to the warming algorithm, image-reference parsing, or registry-map handling land once in `internal/processor` / `internal/parser` and apply to both binaries.

---

## Publish the container image

See `deploy/github-setup.md` for GHCR auth setup. The workflow at `.github/workflows/build.yaml` builds multi-arch images (amd64 + arm64) on every push to `main` and every semver tag.

## Publish standalone binaries

Tagging a release with `vX.Y.Z` triggers `.github/workflows/release.yaml`, which builds both arch binaries plus the universal archive and attaches them to the GitHub Release alongside a `SHA256SUMS` file.

## License

Apache 2.0
