# Supported Resources

## core/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `nodes` | get, list | Synthetic single node with real machine stats |
| `namespaces` | get, list, create, delete | Creates a `q8s-{ns}.network` Podman network |
| `pods` | get, list, create, patch, delete | Writes a `.container` Quadlet; patch rewrites and restarts |
| `services` | get, list, create, patch, delete | Socket units per port |
| `persistentvolumeclaims` | get, list, create, patch, delete | Named Podman volume |
| `configmaps` | get, list, create, update, patch, delete | Files at `{configDir}/{ns}/{name}/` |
| `secrets` | get, list, create, patch, delete | Files at `{secretDir}/{ns}/{name}/` (mode 0600) |
| `events` | get, list, watch | In-memory, last 500, server-generated |

## apps/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `deployments` | get, list, create, patch, delete, scale | Indexed `.container` units; scale, rollout restart, set env |
| `daemonsets` | get, list | Empty stub |
| `statefulsets` | get, list | Empty stub |
| `replicasets` | get, list | Empty stub |

## batch/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `jobs` | get, list, create, patch, delete | `Restart=no`, oneshot |
| `cronjobs` | get, list, create, patch, delete | `.container` + `.timer` unit |

## networking.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `ingresses` | get, list, create, patch, delete | Metadata-only, no proxy/port mapping |

## metrics.k8s.io/v1beta1

| Resource | Notes |
|---|---|
| `nodes` | Real CPU (from /proc/stat) and memory usage |
| `pods` | Per-pod stats from podman stats |

## coordination.k8s.io/v1

| Resource | Notes |
|---|---|
| `leases` | Synthetic node lease for kubectl describe node |

## Pod subresources

| Subresource | kubectl command | Notes |
|---|---|---|
| `log` | `kubectl logs [-f] [--tail N] [--timestamps]` | Streams `podman logs` |
| `exec` | `kubectl exec [-it] pod -- cmd` | WebSocket v5.channel.k8s.io |

## Features

- **Watch**: all list endpoints support `?watch=true` (kubectl get -w, Freelens, k9s)
- **Label selectors**: `key=value`, `key==value`, `key!=value`, `key`, `!key` on all list endpoints
- **Patch**: JSON merge patch and strategic merge patch (array-merge-by-name for containers/env/volumes)
- **Resource limits**: `resources.limits.memory` → Quadlet `Memory=`, `resources.limits.cpu` → `--cpus=N`
- **CrashLoopBackOff**: detected from restart count + non-zero exit, shown in pod status
- **StartLimitBurst=5**: systemd gives up after 5 failures within 60s
- **Delete cascade**: deployment delete removes owned pods, stops units, removes quadlets
- **Deployment restore**: on restart, deployment recreated from container labels if missing
