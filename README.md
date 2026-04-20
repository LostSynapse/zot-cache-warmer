# Zot Cache Warmer

Kubernetes CronJob that ensures every container image deployed to the cluster is cached in a Zot pull-through registry, for ISP outage resilience and as a workaround for Zot issue #2584 (`tag@digest` pull-through bug).

**Prerequisites:** k3s / Kubernetes 1.33+, Zot registry configured as a pull-through cache, ability to push images to a registry (GHCR by default).

**Deploy:** `kubectl apply -f deploy/rbac.yaml -f deploy/cronjob.yaml`

**Configure:** Update the `go.mod` module path, then edit `deploy/cronjob.yaml` and `deploy/rbac.yaml` and replace every `# CHANGEME:` value — see inline comments for details and `deploy/example-values.yaml` for common scenarios.

**Publish the image:** See `deploy/github-setup.md` for GHCR setup; the workflow at `.github/workflows/build.yaml` builds multi-arch images (amd64 + arm64) on every push to `main` and semver tag.

**License:** Apache 2.0
