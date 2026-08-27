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
    discovery.go  API discovery, /version, /api/v1/nodes, storage.k8s.io/v1/storageclasses
    metrics.go    metrics.k8s.io (kubectl top), coordination.k8s.io (lease)
    auth.go       mTLS client certificate middleware
    table.go      Table response rendering for kubectl get
    selector.go   Label selector filtering
    exec.go       kubectl exec WebSocket handler
    validate.go   Request validation

  store/
    store.go      In-memory resource store with JSON persistence

  quadlet/
    generator.go  Quadlet file generators (Container, JobContainer, CronContainer, CronTimer, Volume, Network)
    validate.go   Input sanitization for generated unit files

  systemd/
    manager.go    D-Bus Manager: start/stop/enable/disable/reload units
    activation.go Socket activation (LISTEN_FDS) and default listeners

  podman/
    podman.go     Thin wrapper around `podman ps --format json`
    events.go     Podman event stream (all container lifecycle events, with reconnect backoff)

pkg/
  install/
    install.go    One-shot installer: cert generation, systemd unit creation
```

## State persistence

The store is serialized to `{dataDir}/store.json` on every mutation (atomic write: `.tmp` + rename). On startup, state is loaded from this file. The default namespace is always ensured to exist.

## Sync loop

A background goroutine reconciles state from Podman/systemd every 30 seconds, plus immediately on container events (with 200ms debounce):

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

## Directory layout

```
~/.local/share/q8s/                  # dataDir (rootless)
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
