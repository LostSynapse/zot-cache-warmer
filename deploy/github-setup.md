# GitHub Container Registry (GHCR) Publishing Setup

The `.github/workflows/build.yaml` workflow publishes multi-arch images to
`ghcr.io/<owner>/zot-cache-warmer`. Setup depends on how you host the repo.

## Option A — Fork or own repo under a user or org (recommended)

GitHub Actions ships with an automatic `GITHUB_TOKEN` that already has
`packages: write` scope when the workflow declares it, which this one does.
No manual token creation is required.

1. Fork or clone this repo to your own GitHub user or organization.
2. Enable GitHub Actions: **Settings → Actions → General → Allow all actions
   and reusable workflows**.
3. (First push only) push a commit to `main` to trigger the `build` workflow.
4. After the first successful run, the package appears at
   `https://github.com/users/lostsynapse/packages/container/package/zot-cache-warmer`
   (or the organization equivalent).
5. Make the package public if you want anyone to pull it: **Package page →
   Package settings → Change visibility → Public**. Private packages require
   an `imagePullSecret` in the CronJob.

## Option B — Push to a registry other than GHCR

Replace the `Log in to GitHub Container Registry` step and the `REGISTRY`
env var in `.github/workflows/build.yaml`:

```yaml
env:
  REGISTRY: registry.example.com
  IMAGE_NAME: mygroup/zot-cache-warmer

- name: Log in to my registry
  uses: docker/login-action@v3
  with:
    registry: ${{ env.REGISTRY }}
    username: ${{ secrets.REGISTRY_USERNAME }}
    password: ${{ secrets.REGISTRY_PASSWORD }}
```

Then add the corresponding repository secrets: **Settings → Secrets and
variables → Actions → New repository secret**:

- `REGISTRY_USERNAME` — registry user with push permission
- `REGISTRY_PASSWORD` — registry password or access token

## Tags produced

For every successful build on `main` or a tag push, the workflow publishes:

- `latest` — only on the default branch
- `main` — when built from the `main` branch
- `sha-<shortsha>` — every build
- `v1.2.3`, `1.2`, `1` — on semver tag pushes

Pull requests only run the `test` job; images are not pushed.

## Updating the CronJob image reference

After the first successful publish, update `deploy/cronjob.yaml`:

```yaml
image: ghcr.io/lostsynapse/zot-cache-warmer:latest
```

Or, for production, pin by digest:

```yaml
image: ghcr.io/lostsynapse/zot-cache-warmer:v1.0.0@sha256:<digest>
```

The digest is printed in the workflow summary after a successful build.

## Private GHCR packages in Kubernetes

If the package is kept private, create an image pull secret in the target
namespace:

```bash
kubectl -n <namespace> create secret docker-registry ghcr-credentials \
  --docker-server=ghcr.io \
  --docker-username=<GITHUB_USERNAME> \
  --docker-password=<GITHUB_PAT_WITH_read:packages_SCOPE>
```

Then uncomment `imagePullSecrets` in `deploy/cronjob.yaml`.
