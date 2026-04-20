# Zot Pull-Through Registry — Operator Guide

This deployment runs [Zot](https://zotregistry.dev/) as a pull-through cache for every upstream container registry the cluster depends on. The combined effect: every node pulls through Zot instead of hitting upstream registries directly, which keeps workloads running during ISP outages, reduces external rate-limit exposure, and eliminates repeated pulls of the same image across nodes.

---

## Architecture

Three configuration surfaces must agree on the upstream → Zot-local-path mapping. Changing any one of them without the others breaks image pulls on the node, warming in the CronJob, or both.

```
 ┌──────────────────────────────────────┐
 │ /etc/rancher/k3s/registries.yaml     │  on every k3s node
 │   docker.io → zot/v2/docker-images   │
 └──────────────┬───────────────────────┘
                │  containerd sends pull here
                ▼
 ┌──────────────────────────────────────┐
 │ Zot ConfigMap (deployment.yaml)      │
 │   sync.registries[].content[]        │
 │     prefix: **                       │
 │     destination: /docker-images      │
 └──────────────┬───────────────────────┘
                │  Zot pulls from upstream, stores under /docker-images
                ▼
 ┌──────────────────────────────────────┐
 │ Cache warmer CronJob                 │
 │   ZOT_REGISTRY_MAP=                  │
 │     docker.io=docker-images,...      │
 │   Hits the same /docker-images path  │
 │   pre-emptively to populate Zot.     │
 └──────────────────────────────────────┘
```

**The mapping is: upstream hostname → Zot-local path prefix (no leading slash in env vars, leading slash in Zot config).** Every registry the cluster uses must appear in all three files, with the same destination name.

---

## Current registries

| Upstream host | Zot destination | Example image |
|---|---|---|
| `docker.io` (via `registry-1.docker.io`) | `/docker-images` | `library/nginx:1.25` |
| `ghcr.io` | `/ghcr-images` | `prometheus/node-exporter:v1.8` |
| `registry.k8s.io` | `/k8s-images` | `pause:3.9` |
| `quay.io` | `/quay-images` | `prometheus/prometheus:v2.54` |
| `gcr.io` | `/gcr-images` | `distroless/static:nonroot` |
| `code.forgejo.org` | `/forgejo-images` | `forgejo/forgejo:14.0.3-rootless` |
| `lscr.io` | `/lscr-images` | `linuxserver/webtop:latest` |

When a node pulls `docker.io/library/nginx:1.25`, containerd rewrites the request to `https://zot.lost-synapse.com/v2/docker-images/library/nginx/manifests/1.25`. Zot serves from cache if present, otherwise pulls from `registry-1.docker.io/library/nginx:1.25` synchronously, stores it under `/docker-images/library/nginx`, and returns the manifest. Subsequent pulls of the same image across any node are cache hits.

---

## Adding a new upstream registry

You want to cache images from `example.io` (some hypothetical registry). Four edits, in this order:

### Step 1 — Update Zot's sync configuration

In `deployment.yaml`, append to the `extensions.sync.registries` array:

```json
{
  "urls": ["https://example.io"],
  "onDemand": true,
  "tlsVerify": true,
  "maxRetries": 3,
  "retryDelay": "5s",
  "content": [
    {
      "prefix": "**",
      "destination": "/example-images"
    }
  ]
}
```

Apply:

```bash
kubectl apply -f deployment.yaml
# Zot pod recreates (strategy: Recreate); ~30s downtime while it comes back.
# Existing cached content persists on the Longhorn PVC.
```

### Step 2 — Update the cache warmer's registry map

In the cache-warmer repo's `deploy/cronjob.yaml`, extend the `ZOT_REGISTRY_MAP` env var. Uncomment and edit:

```yaml
- name: ZOT_REGISTRY_MAP
  value: "docker.io=docker-images,ghcr.io=ghcr-images,registry.k8s.io=k8s-images,quay.io=quay-images,gcr.io=gcr-images,code.forgejo.org=forgejo-images,lscr.io=lscr-images,example.io=example-images"
```

Apply:

```bash
kubectl -n zot apply -f deploy/cronjob.yaml
```

### Step 3 — Update the k3s mirror config on every node

Append to `/etc/rancher/k3s/registries.yaml` on each k3s node:

```yaml
mirrors:
  example.io:
    endpoint:
      - https://zot.lost-synapse.com/v2/example-images
```

### Step 4 — Restart k3s on each node so containerd reloads the mirror list

```bash
# On each k3s server and agent, one at a time to avoid full-cluster downtime:
sudo systemctl restart k3s            # on server nodes
sudo systemctl restart k3s-agent      # on agent nodes
```

k3s reads `registries.yaml` only at startup. Existing pods keep running on cached images while k3s restarts.

### Step 5 — Verify

```bash
# Manual warmer run picks up workloads using the new registry
kubectl -n zot create job --from=cronjob/zot-cache-warmer verify-example-$(date +%s)
kubectl -n zot logs -l app.kubernetes.io/name=zot-cache-warmer --tail=-1

# Look for "warmed" or "cache hit" entries referencing example.io
# No "skipped_no_mapping" warnings for example.io means step 2 succeeded.

# Check the image is reachable through Zot
curl -sI https://zot.lost-synapse.com/v2/example-images/<repo>/manifests/<tag>
# HTTP/2 200 = in cache; 404 = not yet pulled; connection error = upstream unreachable
```

---

## Removing an upstream registry

Reverse order of adding: remove the entry from each file, apply, restart k3s, then optionally wipe cached content (`kubectl -n zot exec deploy/zot -- rm -rf /var/lib/zot/<destination>`). Zot's garbage collection (`gc: true, gcInterval: 24h` in the ConfigMap) will eventually purge orphaned blobs on its own.

---

## Migrating from flatten-layout to destination-layout

If an earlier version of this deployment ran without `destination` set on the sync registries, images were stored at Zot's root namespace (e.g. `/library/nginx`). After moving to destination-layout, new pulls go to `/docker-images/library/nginx`. The old content is orphaned but not deleted.

Two options:

1. **Do nothing.** Zot's garbage collector runs every 24h (see `gc` section of the ConfigMap) and will reclaim unreferenced blobs automatically. You'll temporarily use extra disk until the next GC cycle.
2. **Manually clean.** `kubectl -n zot exec deploy/zot -- ls /var/lib/zot` — remove any top-level directories that aren't your configured destinations (`docker-images`, `ghcr-images`, etc.). This is fine because Zot's only references to those paths come from sync.registries.content[].destination; removing a top-level dir that isn't in the current config cannot break anything live.

---

## Troubleshooting

**Node can't pull: `Error response from daemon: pull access denied`**
Check `/etc/rancher/k3s/registries.yaml` on the affected node includes the upstream host and points at the correct Zot destination path. Restart k3s after editing.

**Warmer run reports `skipped_no_mapping: N`**
N images have registries not listed in `ZOT_REGISTRY_MAP`. Run `kubectl -n zot logs -l app.kubernetes.io/name=zot-cache-warmer` and grep `registry not in ZOT_REGISTRY_MAP` to find the hosts — add each to all three config surfaces.

**Warmer run reports `warm_errors: N` with `NAME_UNKNOWN`**
The Zot sync config has no entry for that upstream, OR the upstream has no such repository. Verify the registry appears in `extensions.sync.registries` in `deployment.yaml` and that the image actually exists on that upstream.

**Warmer run reports `probe_errors: N` with `timeout awaiting response headers`**
Normal on first-contact with a large image — Zot is synchronously pulling from upstream and hasn't responded with headers yet. The warmer falls through to the warm path (10-minute context) after a probe timeout, so these usually become successful warms on the same run. If they don't, upstream rate-limiting or large-blob transfer time may have exceeded the 10-minute warm budget; rerun.

**Pods stuck in `ImagePullBackOff` after adding a new upstream**
The pod scheduled before k3s was restarted on its node. Delete the pod to force rescheduling; the new containerd config picks up the mirror.

---

## When everything reaches steady state

After the workloads stabilize and the cache warmer has run a few times:

- Cache warmer's `warmed` counter approaches zero (nothing left to pull)
- `already_cached` counter approaches `images_total` (everything is warm)
- Zot storage usage plateaus (only new image versions add content)
- Node pulls are effectively free of external network I/O

From that point, the cache is doing its job: a 24-hour ISP outage costs nothing because every image the cluster wants is already local. New deployments that introduce new images will trigger a single upstream pull through Zot; every subsequent pod using the same image is a cache hit.

---

## File inventory

| File | Purpose | Updated when |
|---|---|---|
| `deployment.yaml` (this repo) | Zot server, PVC, Service, Ingress, sync config | Adding/removing upstream registries |
| [zot-cache-warmer repo](https://github.com/lostsynapse/zot-cache-warmer) | Cache warmer CronJob + RBAC | Same events (`ZOT_REGISTRY_MAP` env var must stay in sync) |
| `/etc/rancher/k3s/registries.yaml` (each node) | Node-side mirror routing | Same events (rollout node-by-node) |
