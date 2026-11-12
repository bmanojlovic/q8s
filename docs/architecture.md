# q8s Architecture

## How it works

```
kubectl  ─── HTTPS/mTLS ──▶  q8s serve  ──▶  Quadlet files (.container / .network / .volume / .timer)
                                              └──▶  systemd daemon-reload  ──▶  Podman
```

When you `kubectl apply` a Pod, q8s writes a Podman Quadlet `.container` file and tells systemd to reload. systemd then starts the container via Podman. Every resource maps directly to a native Linux primitive:

| Kubernetes resource | Linux primitive |
|---|---|
| Namespace | Podman network (`q8s-{ns}.network`) |
| Pod | Quadlet `.container` unit |
| Deployment | N indexed Quadlet `.container` units (`{name}-0` … `{name}-N`) with `Restart=on-failure` |
| PersistentVolumeClaim | Quadlet `.volume` unit (named Podman volume) or host bind mount |
| ConfigMap | Directory of files bind-mounted into containers |
| Secret | Directory of files (mode 0600) bind-mounted into containers |
| Job | Quadlet `.container` unit with `Restart=no` |
| CronJob | Quadlet `.container` unit + systemd `.timer` unit |
| Node | Synthetic — real machine stats (hostname, CPU, RAM, disk, kernel) |

## Package layout

```
cmd/
  q8s/            Single binary: serve / install / uninstall / status

internal/
  server/
    server.go     HTTP server, TLS setup, route registration
    handler.go    All resource handlers (Pod, Service, PVC, ConfigMap, Secret, Deployment, Job, CronJob)
    discovery.go  API discovery, /version, /api/v1/nodes, storage.k8s.io/v1/storageclasses, apiextensions.k8s.io
    crds.go       Embedded read-only CustomResourceDefinition list and table
    traefik.go    traefik.io/v1alpha1 Middleware and IngressRouteTCP handlers, rendering
    traefik_validate.go  Validation for the Traefik CRD subset
    apply.go      Server-side apply translation to create/merge-patch
    openapi_v2.json  Generated Swagger 2.0 schema for client-side validation
    crds.json     Generated Traefik CRD list
    metrics.go    metrics.k8s.io (kubectl top), coordination.k8s.io (lease)
    auth.go       mTLS client certificate middleware
    table.go      Table response rendering for kubectl get
    selector.go   Label selector filtering
    exec.go       kubectl exec WebSocket handler
    validate.go   Request validation

  traefik/
    types.go      Traefik v1alpha1 Middleware and IngressRouteTCP subset types
    deepcopy.go   Hand-written DeepCopy/DeepCopyObject for runtime.Object

  store/
    store.go      In-memory resource store with JSON persistence

hack/
  gen-openapi/    Generator for internal/server/openapi_v2.json and crds.json

  quadlet/
    generator.go  Quadlet file generators (Container, JobContainer, CronContainer, CronTimer, Volume, Network)
    validate.go   Input sanitization for generated unit files

  systemd/
    manager.go    D-Bus Manager: start/stop/enable/disable/reload units
    watch.go      D-Bus PropertiesChanged subscription: triggers the sync loop
    activation.go Socket activation (LISTEN_FDS) and default listeners

  podman/
    podman.go     Thin wrapper around `podman ps --format json`

pkg/
  install/
    install.go    One-shot installer: cert generation, systemd unit creation
```

## State persistence

The store is serialized to `{dataDir}/store.json` on every mutation (atomic write: `.tmp` + rename). On startup, state is loaded from this file. The default namespace is always ensured to exist.

On startup q8s also re-renders the quadlet file of every stored Pod and Deployment instance. A file whose rendering is unchanged is left alone. A file that changed (typically after a q8s upgrade that renders units differently) is rewritten, and if its service is running it is restarted so the upgrade reaches it; a stopped or finished unit only gets the new file. To apply a change to a running pod without restarting q8s, use `kubectl rollout restart deployment/<name>`.

## Sync loop

A background goroutine reconciles state from Podman/systemd every 30 seconds, plus immediately when systemd reports a unit property change. Those changes arrive as `PropertiesChanged` D-Bus signals from the systemd manager, on a dedicated connection that subscribes with a narrow match rule (unit objects only); no child process is spawned and the journal is never read. Only units whose quadlet source exists in the quadlet directory trigger a pass, and a 200 ms debounce collapses the burst of changes one container transition produces. The 30 s ticker stays as a correctness backstop in case a signal is missed (for example during a bus reconnect).

| systemd `ActiveState` | Pod phase |
|---|---|
| `active` | Running |
| `activating` (with restarts) | Running (shows CrashLoopBackOff) |
| `inactive` + result `success` | Succeeded |
| `inactive` + result `exit-code/signal/…` | Failed |
| `failed` | Failed |
| anything else | Pending |

Deployment-owned pods are restored from container labels on startup if the deployment is missing from the store.

## Node endpoint

`/api/v1/nodes` returns a single synthetic node built from real system data:

- Hostname: `os.Hostname()`
- Internal IP: first non-loopback IPv4 from `net.Interfaces()`
- Kernel: `unix.Uname()`
- OS image: `PRETTY_NAME` from `/etc/os-release`
- Podman version: `podman --version`
- CPU: `runtime.NumCPU()`
- Memory: `unix.Sysinfo().Totalram`
- Disk: `unix.Statfs("/")`
- Machine ID: `/etc/machine-id`
- Boot ID: `/proc/sys/kernel/random/boot_id`
- Boot time (age): `unix.Sysinfo().Uptime`

## Version endpoint

`/version` derives the Kubernetes API version from the `k8s.io/apimachinery` module version embedded in the binary via `runtime/debug.ReadBuildInfo()`. Go version, compiler, and platform come from `runtime.*`. The version is suffixed with `+q8s`.

## Metrics

`/apis/metrics.k8s.io/v1beta1/nodes` provides real CPU and memory usage:
- CPU: samples `/proc/stat` twice (100ms apart), computes nanocores
- Memory: `MemTotal - MemAvailable` from `/proc/meminfo`

`/apis/metrics.k8s.io/v1beta1/pods` runs `podman stats --no-stream` per container.

## Security

All API traffic uses mutual TLS (mTLS). No certificate = 401. Certificates are ECDSA P-256, valid 1 year. Having the client cert implies full access (single-user system, no RBAC).

## User namespaces (rootful)

Rootful q8s runs every pod in its own user namespace, the podman equivalent of
Kubernetes `pod.spec.hostUsers: false` (KEP-127). Standalone containers, Jobs
and CronJobs get `UserNS=auto:size=65536` in their `[Container]` section; a
multi-container pod gets it once in its `[Pod]` section, because podman rejects
a user namespace on a container that joins a pod and members inherit the pod's
namespace.

`UserNS=auto` makes podman allocate a fresh, unprivileged host uid range for the
pod from the `containers` entry in `/etc/subuid` and `/etc/subgid`. Root inside
the container is therefore not host root, and two containers using the same uid
no longer share a host identity. `size=65536` asks for 65536 ids per pod. The
size is explicit because podman's auto sizing otherwise takes the highest uid
and gid in the image's `/etc/passwd` and `/etc/group` with a minimum of 1024, so
a small image such as `python:alpine` gets only 1024 ids and a job that chowns a
shared volume file to uid 2000 fails with `EINVAL`.

On startup rootful `q8s serve` ensures the range exists
(`containers:2147483647:2147483648`) by appending it when missing. The choice is
deliberately high so it cannot collide with ordinary login users, and existing
lines are never rewritten. The pool holds 2^31 ids, so 65536 per pod costs
nothing in practice.

Override the size with the `q8s.io/userns-size` annotation on the pod (for
Deployments, Jobs and CronJobs, on the pod template metadata):

```yaml
metadata:
  annotations:
    q8s.io/userns-size: "200000"
```

The value must be a decimal integer between 1024 and 16777216. The default is
65536, and the API rejects an invalid value instead of silently using the
default.

Every `Volume=` line of a unit in a user namespace gets the `idmap` mount option
(named volumes, hostpath binds, ConfigMap and Secret binds, with or without
`:ro`). Without it the shifted uids would see the host owners of existing data
as unmapped; with it, files owned by host uid 0 or 2000 appear as 0 or 2000
inside the container. Data keeps its host owners and no chown is needed.

The memory-backed emptyDir at `/dev/shm` is not a `Volume=` line and stays as
rendered (`ShmSize=` when a size limit is set).

Two exceptions stay in the host user namespace:

- `hostNetwork: true` pods. A user namespace with the host network namespace
  gains no isolation and cannot bind host ports below 1024.
- `hostUsers: true` pods, the Kubernetes opt-out. `hostUsers: false` is the
  same as leaving it unset: the pod gets a user namespace. This is the way to
  opt out completely; the `q8s.io/userns-size` annotation only changes the
  size.

Rootless mode is unchanged: rootless podman containers already run inside the
user's subordinate uid range.

## Directory layout

```
~/.local/share/q8s/                  # dataDir (rootless)
  config.json                        # persistent install config (port, serverURL, SANs)
  store.json                         # persisted resource state
  certs/                             # TLS certificates

~/.config/containers/systemd/        # Podman quadlet directory
  q8s-default.network
  default-nginx.container

~/.config/systemd/user/              # systemd user unit directory
  q8s.socket
  q8s-api.service
  default-my-cron-cron.timer

$XDG_RUNTIME_DIR/q8s/               # runtime (tmpfs)
  api.sock
  configmaps/{ns}/{name}/{key}
  secrets/{ns}/{name}/{key}          # mode 0600
```

## Design boundary: translate, don't operate

q8s is deliberately controller-like but minimal. It *translates* Kubernetes
API objects into local primitives (quadlets, systemd units, Traefik config
files) and reconciles them; it does not take ownership of lifecycles that
another component already owns. Consequences:

- **Public TLS** is Traefik's (`secretName` on Ingress is accepted, not read).
- **Service fan-out** is Traefik's `loadBalancer.servers`, kept in sync from
  selectors — q8s doesn't proxy traffic itself.
- **DNS/ACME/cert renewal, RBAC, scheduling, multi-node** — out of scope by
  design; if a feature needs q8s to become a participant in a runtime path,
  the default answer is no.
