# Supported Resources

Every pod spec q8s accepts must contain exactly one container; specs with additional containers, initContainers, or ephemeralContainers are rejected.

## core/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `nodes` | get, list | Synthetic single node with real machine stats |
| `namespaces` | get, list, create, patch, delete | Creates a `q8s-{ns}.network` Podman network; patch updates labels and annotations |
| `pods` | get, list, create, patch, delete | Writes a `.container` Quadlet; patch rewrites and restarts |
| `services` | get, list, create, patch, delete | Selector + port map: DNS aliases (NetworkAlias) and ingress backend resolution; never binds a host port |
| `persistentvolumeclaims` | get, list, create, patch, delete | Named Podman volume; storageClass selects mount mode |
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
| `ingresses` | get, list, create, patch, delete | Traefik dynamic config per ingress; backends resolved from the Service selector (one server per matching pod's hostPort, like Endpoints) |

## traefik.io/v1alpha1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `middlewares` | get, list, create, update, patch, delete | HTTP `ipAllowList`, `redirectRegex`, `redirectScheme` and `headers` subsets; one type per object, rendered as a Traefik file-provider middleware |
| `ingressroutetcps` | get, list, create, update, patch, delete | Catch-all `HostSNI(`*`)` TCP routers with optional PROXY protocol and TLS passthrough; an entrypoint may be owned by only one object |

See [Traefik Middleware and IngressRouteTCP](traefik.md) for the supported
subset, generated files and the `@kubernetescrd` to `@file` middleware
reference rewrite.

## metrics.k8s.io/v1beta1

| Resource | Notes |
|---|---|
| `nodes` | Real CPU (from /proc/stat) and memory usage |
| `pods` | Per-pod stats from podman stats |

## coordination.k8s.io/v1

| Resource | Notes |
|---|---|
| `leases` | Synthetic node lease for kubectl describe node |

## storage.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `storageclasses` | get, list | Fixed set: `standard`, `standard-shared`, `hostpath` |

### Storage classes

| Class | Volume type | SELinux | Use case |
|---|---|---|---|
| `standard` (default) | Podman named volume | `:Z` (exclusive) | Single-pod volumes |
| `standard-shared` | Podman named volume | `:z` (shared) | Multiple pods sharing a volume |
| `hostpath` | Bind mount from host | `:Z` (exclusive) | Pre-existing host directories |

PVCs bind immediately at creation (`status.phase: Bound`) — the backing
volume is a quadlet file, so there is no asynchronous provisioner to wait
on. A claim without `storageClassName` is defaulted to `standard`, matching
the `is-default-class` annotation on that class. For `standard`/`standard-shared`
the named Podman volume is created right away (the volume unit is started at
PVC creation); `hostpath` claims bind to the directory named in their
annotation.

Named volumes are namespace-scoped as `{namespace}-{name}` (also surfaced
in `spec.volumeName`), so same-named claims in different namespaces — or a
claim colliding with a volume some unrelated non-q8s deployment created —
never share one backing volume, and `podman volume ls` shows which
namespace/claim owns each volume.

The `hostpath` class requires a `q8s.io/host-path` annotation on the PVC specifying the absolute host directory. It also honours an optional `q8s.io/selinux-label` annotation: `private` (the default, `:Z`) relabels the host directory for one container, while `shared` (`:z`) lets several containers mount it without locking each other out. Any other value is rejected.

Mounts with `readOnly: true`, or PVC volume sources with `persistentVolumeClaim.readOnly: true`, get the Podman `ro` mount option (`:ro,Z`, `:ro,z`, or `:ro`).

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  annotations:
    q8s.io/host-path: "/srv/data/myapp"
spec:
  storageClassName: hostpath
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
```

## apiextensions.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `customresourcedefinitions` | get, list | Read-only fixed list of `middlewares.traefik.io` and `ingressroutetcps.traefik.io`, generated from the Traefik CRD file |

CRDs are not stored or modified by q8s. The list exists so clients that
build their type information from CRD schemas (Terraform's
`kubernetes_manifest`, for example) can plan Traefik Middleware and
IngressRouteTCP objects.

## Pod subresources

| Subresource | kubectl command | Notes |
|---|---|---|
| `log` | `kubectl logs [-f] [--tail N] [--timestamps]` | Streams `podman logs` |
| `exec` | `kubectl exec [-it] pod -- cmd` | WebSocket v5.channel.k8s.io |

## Features

- **Watch**: all list endpoints support `?watch=true` (kubectl get -w, Freelens, k9s)
- **Label selectors**: `key=value`, `key==value`, `key!=value`, `key`, `!key` on all list endpoints
- **Patch**: JSON merge patch and strategic merge patch (array-merge-by-name for containers/env/volumes); RFC 6902 JSON Patch (`application/json-patch+json`) on all resources; Terraform's provider patches Secret `data` keys and container/spec changes this way
- **Server-side apply**: `PATCH` with `Content-Type: application/apply-patch+yaml` is translated to a create or merge patch. q8s has no managed fields, so apply uses a single-manager model: the applier owns `spec`, `data`, `binaryData`, `stringData`, `metadata.labels` and `metadata.annotations`. `fieldManager` and `force` are accepted and ignored, and `dryRun` is rejected
- **Resource limits**: `resources.limits.memory` → `PodmanArgs=--memory=N --memory-swap=-1`, `resources.limits.cpu` → `PodmanArgs=--cpus=N` (via PodmanArgs for podman-version portability; skipped when cgroup controllers not delegated)
- **GPU**: `resources.limits["nvidia.com/gpu"]` of at least 1 → `AddDevice=nvidia.com/gpu=all` (CDI; podman resolves the GPU devices). The count is not allocated, so any request of 1 or more exposes every CDI GPU on the host.
- **Shared memory**: an `emptyDir` with `medium: Memory` mounted at `/dev/shm` → `ShmSize=<bytes>` from `sizeLimit`; no `Volume=` line is generated for it. Without a `sizeLimit`, nothing is emitted.
- **Ulimits**: pod annotation `q8s.io/ulimits` (for example `memlock=-1:-1,nofile=65536`) → one `Ulimit=<item>` line per comma separated item. Items must match `name=soft[:hard]` and are rejected otherwise.
- **Health checks**: `livenessProbe` owns the container's single podman health check (`HealthCmd=` in JSON exec form, plus `HealthOnFailure=kill` so the unit's `Restart=` actually restarts a failed container); otherwise `readinessProbe` supplies a status-only check. Both render `HealthInterval=` (from `periodSeconds`, default 10s), `HealthTimeout=` (from `timeoutSeconds`, default 1s), `HealthRetries=` (from `failureThreshold`, default 3) and `HealthStartPeriod=` (from `initialDelaySeconds`, only when set). An httpGet probe becomes a small `sh` script that requests `<scheme>://127.0.0.1:<port><path>` with the first client the image has (curl, else wget, else python3), so it needs `/bin/sh` and one of those in the image (distroless images need an exec probe); a named port is resolved against the container's ports and an unknown name is an error. `startupProbe` maps to `HealthStartupCmd=` and its own timing keys. `tcpSocket`, `grpc`, unsupported ports/headers/hosts and a `startupProbe` without a main probe are rejected with 422. See [configuration.md](configuration.md#health-checks-probes).
- **Port semantics**: `containerPort` is internal-only (namespace network); `hostPort` publishes to host via `PublishPort=`; a Service never binds a port, it maps selector + `targetPort` to DNS aliases and ingress backends. Ingress backends reach non-hostNetwork pods by their `<pod>.<ns>` DNS name on the namespace network; an explicit `hostPort` keeps its host publish.
- **CrashLoopBackOff**: detected from restart count + non-zero exit, shown in pod status
- **StartLimitBurst=5**: systemd gives up after 5 failures within 60s
- **Delete cascade**: deployment delete removes owned pods, stops units, removes quadlets
- **Deployment restore**: on restart, deployment recreated from container labels if missing
