# q8s Architecture

## How it works

```
kubectl  ─── HTTPS/mTLS ──▶  q8s serve  ──▶  Quadlet files (.container / .network / .volume / .timer)
                                              └──▶  systemd daemon-reload  ──▶  Podman
```

When you `kubectl apply` a Pod, q8s writes a Podman Quadlet `.container` file (a `.pod` file plus one `.container` per member for a multi-container pod) and tells systemd to reload. systemd then starts the container via Podman. Every resource maps directly to a native Linux primitive:

| Kubernetes resource | Linux primitive |
|---|---|
| Namespace | Podman network (`q8s-{ns}.network`) |
| Pod | Quadlet `.container` unit, plus a `.pod` unit when the pod has more than one container |
| Deployment | N indexed pods (`{name}-0` … `{name}-N`), each a `.container` unit or, when multi-container, a `.pod` unit with its containers, with `Restart=on-failure` |
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
    auth.go       mTLS client certificate middleware and request identity
    authorizer.go Per-request RBAC authorization
    rbac.go       Role, RoleBinding, ClusterRole and ClusterRoleBinding handlers
    rbac_defaults.go  Built-in ClusterRoles
    authz.go      SelfSubjectAccessReview, SelfSubjectRulesReview, SubjectAccessReview and SelfSubjectReview
    impersonation.go  Impersonate-User and Impersonate-Group handling
    revocation.go Revoked certificate cache
    requestinfo.go    Request path to RBAC attributes
    workload.go   ServiceAccount credential socket
    table.go      Table response rendering for kubectl get
    selector.go   Label selector filtering
    exec.go       kubectl exec WebSocket handler
    validate.go   Request validation

  traefik/
    types.go      Traefik v1alpha1 Middleware and IngressRouteTCP subset types
    deepcopy.go   Hand-written DeepCopy/DeepCopyObject for runtime.Object

  store/
    store.go      In-memory resource store with synchronous JSON persistence and snapshots

  fsutil/
    fsutil.go     Atomic file writes (temporary file, fsync, rename)

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

Every store mutation is written to `{dataDir}/store.json` before the API
answers. The write is synchronous and atomic: a temporary file in the same
directory, `fsync`, then `rename`. Each save logs one line with the
resourceVersion that reached disk:

```text
store: saved rv 42 (128 KB, 3 ms, snapshot 12 KB)
```

Mutations are serialized with their save: no other change can happen between
a mutation and its write. A failed write (full disk, I/O error) answers 500
`InternalError` and rolls the in-memory state back to the last state that
reached disk. Deletes stop the units and remove their files first, then
delete from the store. If that final save fails, nothing is restored: the
object stays in the store with its units stopped and its files gone, and the
request answers 500 `InternalError` with `<kind> "<name>" was stopped but
could not be removed from the store (<err>); retry the delete`. A retried
DELETE stops nothing new and removes the store entry. The resourceVersion
keeps moving forward, so a version already handed to a client is never
reused.

A successful save also keeps a gzip copy in `{dataDir}/snapshots` as
`store.<rv>.json.gz` when the newest snapshot is at least a minute old (the
log line then says `no snapshot`); the newest 20 are kept, so they reach back
at least 20 minutes. When a save skips its snapshot, one timer is armed for
the rest of the interval and then snapshots the last state that reached disk,
so the end of a burst is not lost. A snapshot failure is logged but
does not fail the save. If `store.json` is missing or does not parse while
snapshots exist, startup loads the newest readable snapshot, renames the
broken file to `store.json.corrupt-<unix time>` and rewrites `store.json`.
Each delete advances the resourceVersion as well, so a version names exactly
one state.

Generated files are written atomically too, through `internal/fsutil`
(temporary file in the same directory, `fsync`, `rename`): Quadlet units,
systemd units, ConfigMap and Secret data files, Secret-derived env files,
Traefik files, the workload kubeconfig and the workload binary copy. A crash
or a full disk therefore never leaves a truncated file. ConfigMap and Secret
volumes are directory mounts, so a running container sees the replaced files.
The stale file cleanup ignores leftover dot-prefixed temporary files.

On startup q8s also re-renders the quadlet file of every stored Pod and Deployment instance. A file whose rendering is unchanged is left alone. A file that changed (typically after a q8s upgrade that renders units differently) is rewritten, and if its service is running it is restarted so the upgrade reaches it; a stopped or finished unit only gets the new file. To apply a change to a running pod without restarting q8s, use `kubectl rollout restart deployment/<name>`.

## Disk guard

While any filesystem q8s writes to has less free space than
`min(100 MiB, 5% of its size)`, `POST`, `PUT` and `PATCH` answer 507
`InsufficientStorage` with the low path, its free space and the threshold.
The check covers the store, Quadlet, systemd, ConfigMap and Traefik
directories, is cached for 2 seconds, and walks up to the nearest existing
ancestor of a path that does not exist yet. `DELETE`, pod `exec`, `attach` and
`portforward` are not blocked, so space can still be freed.

The node's `DiskPressure` condition comes from the same check instead of being
hard-coded: `True` with reason `KubeletHasDiskPressure` and the low path in
the message while the guard is active, `False` with reason
`KubeletHasNoDiskPressure` otherwise. The node then carries the
`node.kubernetes.io/disk-pressure:NoSchedule` taint, as a kubelet would set
it. `q8s doctor` reports the same state through the same threshold code; see
[docs/doctor.md](doctor.md#disk).

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

A `restartPolicy: Never` pod whose container exits quickly still reaches
`Succeeded`. Its `--rm` container can already be gone when the next status
sync runs, and systemd then reports inactive with `Result=success`, exactly
like a unit that has not started. q8s compares the unit's
`ExecMainStartTimestamp` with the pod's creation time: a unit that started at
or after the pod was created ran for this pod.

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

All API traffic uses mutual TLS (mTLS). No certificate = 401. Certificates are
ECDSA P-256. The administrator certificate from `q8s install` and user
certificates from `q8s kubeconfig --user` default to 1 year; a pod's
ServiceAccount certificate is valid for 1 hour.

The client certificate is the identity: CommonName is the user, Organization
entries are groups, and `system:authenticated` is added. Members of
`system:masters` or `q8s-user` are administrators and bypass authorization.
Every other request is authorized against Roles, RoleBindings, ClusterRoles
and ClusterRoleBindings, with the review endpoints and impersonation described
in [RBAC and user identities](rbac.md). Revoked certificates are refused with
401 before authorization runs. The workload credential socket is local and
unix domain only, and the API itself stays mTLS-only.

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
  config.json                        # persistent install config (name, port, serverURL, SANs)
  store.json                         # persisted resource state
  store.json.corrupt-<time>          # kept after a recovery from a snapshot
  snapshots/store.<rv>.json.gz       # newest 20 snapshots
  certs/                             # TLS certificates
  revoked.json                       # revoked users and serials, after a revocation
  workload-bin/q8s                   # exec credential plugin copy
  traefik/                           # generated Traefik dynamic configuration

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
  workload/kubeconfig
  workload/ns/{ns}/kubeconfig
  workload/sa.sock                   # ServiceAccount credential socket
```

Rootful uses `/etc/q8s` for the data directory and `/run/q8s` for the runtime
directory. `q8s-api.service` sets `RuntimeDirectoryPreserve=yes`, so a q8s
restart or upgrade keeps `/run/q8s` in place. Running pods keep their bind
mounts on the ConfigMap and Secret directories under it; without the setting
systemd would delete the tree on stop and the next start would create new
directories, leaving those pods with empty volumes.

## Design boundary: translate, don't operate

q8s is deliberately controller-like but minimal. It *translates* Kubernetes
API objects into local primitives (quadlets, systemd units, Traefik config
files) and reconciles them; it does not take ownership of lifecycles that
another component already owns. Consequences:

- **Public TLS** is Traefik's (`secretName` on Ingress is accepted, not read).
- **Service fan-out** is Traefik's `loadBalancer.servers`, kept in sync from
  selectors — q8s doesn't proxy traffic itself.
- **DNS/ACME/cert renewal, scheduling, multi-node**: out of scope by
  design; if a feature needs q8s to become a participant in a runtime path,
  the default answer is no.
