# q8s

A Kubernetes-compatible API server that runs containers on a single Linux machine using Podman and systemd Quadlets instead of a cluster. The Kubernetes API surface is deliberately small: enough to use `kubectl` normally, but backed by local container primitives rather than a distributed scheduler.

## How it works

```
kubectl  ─── HTTPS/mTLS ──▶  q8s serve  ──▶  Quadlet files (.container / .network / .volume / .timer)
                                              └──▶  systemd daemon-reload  ──▶  Podman
```

When you `kubectl apply` a Pod, q8s writes a Podman [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) `.container` file and tells systemd to reload. systemd then starts the container via Podman. Every resource maps directly to a native Linux primitive:

| Kubernetes resource | Linux primitive |
|---|---|
| Namespace | Podman network (`q8s-{ns}.network`) |
| Pod | Quadlet `.container` unit |
| Deployment | N indexed Quadlet `.container` units (`{name}-0` … `{name}-N`) with `Restart=on-failure` |
| PersistentVolumeClaim | Quadlet `.volume` unit (named Podman volume) |
| ConfigMap | Directory of files bind-mounted into containers |
| Job | Quadlet `.container` unit with `Restart=no` |
| CronJob | Quadlet `.container` unit + systemd `.timer` unit |

State is persisted to JSON on disk (`store.json`) so resources survive server restarts. ConfigMap and Secret files live on `$XDG_RUNTIME_DIR` (tmpfs) and are restored from the store on startup.

## Requirements

- Linux with systemd (user or system mode)
- Podman 4.4+ (Quadlet support)
- Go 1.26.4+ (to build)
- `kubectl` (any recent version)

## Building

```sh
make          # vet + build
make build    # q8s only
make install  # build then run q8s install
make test     # go test ./...
make clean    # remove binary
```

Produces one binary: `q8s`.

## Installation

```sh
q8s install
```

This:
1. Creates the data directory (`~/.local/share/q8s` rootless, `/etc/q8s` rootful)
2. Generates a self-signed CA plus server and client ECDSA P-256 certificates (valid 1 year), written to `{dataDir}/certs/`
3. Installs two systemd units: `q8s.socket` (socket activation on TCP `:6443` and a Unix socket) and `q8s-api.service`
4. Prints the exact `kubectl config` commands needed

```sh
q8s uninstall  # remove systemd units and reload daemon
q8s status     # check if the socket is active
q8s serve      # run the API server directly (no systemd)
```

## kubectl setup

After `q8s install` prints the commands, run them. Example (rootless):

```sh
kubectl config set-cluster q8s \
  --server=https://localhost:6443 \
  --certificate-authority=~/.local/share/q8s/certs/ca.crt \
  --client-certificate=~/.local/share/q8s/certs/client.crt \
  --client-key=~/.local/share/q8s/certs/client.key \
  --embed-certs=true

kubectl config set-credentials q8s-user --embed-certs=true
kubectl config set-context q8s --cluster=q8s --user=q8s-user
kubectl config use-context q8s
```

## Activating the server

Socket-activated (recommended — systemd starts the server on first connection):

```sh
systemctl --user enable --now q8s.socket
```

Or run directly:

```sh
q8s serve
```

## Supported resources

All resources support `patch` (JSON merge patch / strategic merge patch with array-merge-by-name for containers and env).

### apps/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `deployments` | get, list, create, patch, delete | N indexed `.container` units (`{name}-0` … `{name}-N`); `scale` subresource; `rollout restart` triggers rolling restart |

### core/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `namespaces` | get, list, create, delete | Creates a `q8s-{ns}.network` Podman network |
| `pods` | get, list, create, patch, delete | Writes a `.container` Quadlet; patch rewrites file and restarts unit |
| `services` | get, list, create, patch, delete | Writes a `{name}-{port}.socket` systemd socket unit per port (`ListenStream`); no proxy/forwarding |
| `persistentvolumeclaims` | get, list, create, patch, delete | Writes a `.volume` Quadlet (named Podman volume) |
| `configmaps` | get, list, create, update, patch, delete | Files written to `{configDir}/{ns}/{name}/`; updated on every write |
| `secrets` | get, list, create, patch, delete | Files written to `{secretDir}/{ns}/{name}/` (mode 0600); restored on startup |

### batch/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `jobs` | get, list, create, patch, delete | Quadlet unit with `Restart=no`, no `[Install]` |
| `cronjobs` | get, list, create, patch, delete | `.container` Quadlet + `.timer` unit |

### Pod subresources

| Subresource | kubectl command | Notes |
|---|---|---|
| `log` | `kubectl logs [-f] [--tail N] [--timestamps]` | Streams `podman logs` output |
| `exec` | `kubectl exec pod -- cmd` | WebSocket (`v5.channel.k8s.io`); requires kubectl ≥1.29 |

`kubectl exec -it` works — Podman allocates a PTY inside the container so interactive programs (bash, readline, vim) behave correctly. Terminal resize is not propagated; if you need resize use `podman exec -ti {ns}-{name}` directly.

## Pod → Quadlet mapping

A Pod spec is translated to a `.container` Quadlet file at `{quadletDir}/{ns}-{name}.container`:

```ini
[Container]
Image=nginx:latest
ContainerName=default-nginx          # {ns}-{name}
Exec=...                             # command + args, shell-quoted
WorkingDir=...
User=...                             # from securityContext.runAsUser
Environment=KEY=value
PublishPort=80:80/tcp
Volume=my-pvc:/data                  # PersistentVolumeClaim
Volume=/run/q8s/configmaps/default/config:/etc/config:ro,z  # ConfigMap
Volume=/run/q8s/secrets/default/creds:/run/secrets:ro,z     # Secret
Network=q8s-default.network          # namespace network

[Unit]
Description=Pod nginx

[Service]
Restart=on-failure                   # when restartPolicy=Always or OnFailure
RestartSec=5

[Install]
WantedBy=default.target
```

Quadlet files are placed in:
- **Rootless**: `~/.config/containers/systemd/`
- **Rootful**: `/etc/containers/systemd/`

## Namespace networks

Every namespace gets a dedicated Podman network named `q8s-{namespace}`. Pods in the same namespace share the network. Pods across namespaces are isolated. The `default` namespace network is created on startup; others are created when `kubectl create namespace` runs.

The `default` name is reserved by Podman, so all q8s networks carry the `q8s-` prefix: the network file is `q8s-{ns}.network`, the network name inside it is `q8s-{ns}`, and container units reference it as `Network=q8s-{ns}.network`.

## Deployments

A Deployment creates one Quadlet `.container` unit per replica, named `{ns}-{name}-{i}.container` (zero-indexed). All instances share the same image and spec — replicas behave like a ReplicaSet without pod identity.

`kubectl scale`, `kubectl set env`, `kubectl edit`, and `kubectl rollout restart` all work:

- **Scale up**: new instance quadlets are written and started.
- **Scale down**: excess instance units are stopped and their quadlet files removed.
- **`rollout restart`**: sends a PATCH with a `restartedAt` annotation; q8s rewrites all instance quadlets and calls `RestartUnit` on each in sequence.
- **`set env` / `patch`**: the quadlet for every running instance is rewritten and the unit restarted, so the new env var appears in the running container.

The `deployments/scale` subresource is implemented so `kubectl scale` works directly.

## ConfigMap files

When a ConfigMap is created or updated, each key is written as a file:

```
{configDir}/{namespace}/{configmap-name}/{key}
```

Rootless default: `$XDG_RUNTIME_DIR/q8s/configmaps/` (tmpfs — restored from JSON store on restart).

In a Pod spec, mount a ConfigMap volume as usual:

`envFrom: configMapRef` is also supported — q8s expands it into individual `Environment=` entries in the Quadlet at generation time.

```yaml
volumes:
- name: config
  configMap:
    name: my-config
volumeMounts:
- name: config
  mountPath: /etc/config
```

The Quadlet gets `Volume=/run/user/1000/q8s/configmaps/default/my-config:/etc/config:ro,z`. The `:z` flag applies SELinux relabelling so the container can read the files on enforcing systems.

## Secret files

When a Secret is created or updated, each key is written as a file (mode 0600):

```
{secretDir}/{namespace}/{secret-name}/{key}
```

Rootless default: `$XDG_RUNTIME_DIR/q8s/secrets/` (tmpfs — restored from JSON store on restart).

Mount a Secret volume in the same way as ConfigMap:

```yaml
volumes:
- name: creds
  secret:
    secretName: my-secret
volumeMounts:
- name: creds
  mountPath: /run/secrets
```

The Quadlet gets `Volume=/run/user/1000/q8s/secrets/default/my-secret:/run/secrets:ro,z`.

`envFrom: secretRef` is also supported — expanded into `Environment=` entries at quadlet generation time.

## Jobs

A Job generates a single `.container` Quadlet with `Restart=no` and no `[Install]` stanza (it is started on demand, not by a target). The unit name is `{ns}-{name}-job.service`. Job status (active / succeeded / failed) is synced from systemd every 5 seconds.

## CronJobs

A CronJob generates two units:
- `{ns}-{name}-cron.container` in the Podman quadlet directory (the container to run)
- `{ns}-{name}-cron.timer` in the systemd user unit directory (the schedule)

The timer name matches the container unit name so systemd links them automatically.

### Cron expression translation

The 5-field cron schedule is translated to systemd `OnCalendar=` format:

| Cron | `OnCalendar=` |
|---|---|
| `0 3 * * *` | `*-*-* 3:0:00` |
| `*/5 * * * *` | `*-*-* *:0/5:00` |
| `*/1 * * * *` | `*-*-* *:*:00` |
| `0 0 1 * *` | `*-*-1 0:0:00` |

Step expressions `*/N` become `0/N` (systemd counts from zero). `*/1` simplifies to `*`.

Timer files go to the systemd unit directory (not the Podman quadlet dir, which only processes `.container`, `.volume`, and `.network` files).

## Security

All API traffic uses mutual TLS (mTLS). The server requires a valid client certificate signed by the CA. No certificate → `401 Unauthorized`. The CA, server cert, and client cert are all ECDSA P-256, valid for 1 year.

Certificate files (rootless):

```
~/.local/share/q8s/certs/
  ca.crt      CA certificate (trusted root)
  ca.key      CA private key
  server.crt  Server certificate (SAN: localhost, 127.0.0.1, ::1)
  server.key  Server private key
  client.crt  kubectl client certificate
  client.key  kubectl client private key
```

All key files are written mode `0600`.

## State persistence

The store is serialized to `{dataDir}/store.json` on every mutation (atomic write: `.tmp` + rename). On startup, state is loaded from this file. The default namespace is always ensured to exist.

Write ordering: `saveMu` is acquired before taking the read snapshot, so concurrent saves always capture the latest state — a stale snapshot cannot overwrite a newer one.

## Directory layout

```
~/.local/share/q8s/                  # dataDir (rootless)
  store.json                         # persisted resource state
  certs/                             # TLS certificates

~/.config/containers/systemd/        # Podman quadlet directory
  q8s-default.network
  default-nginx.container
  default-my-deploy.container
  default-my-pvc.volume
  default-my-job-job.container
  default-my-cron-cron.container

~/.config/systemd/user/              # systemd user unit directory
  q8s.socket
  q8s-api.service
  default-my-cron-cron.timer

$XDG_RUNTIME_DIR/q8s/               # runtime directory (tmpfs)
  api.sock                           # Unix socket
  configmaps/
    default/
      my-config/
        config.yaml
  secrets/
    default/
      my-secret/
        password                     # mode 0600
```

Rootful paths use `/etc/q8s`, `/etc/containers/systemd`, `/etc/systemd/system`, and `/run/q8s`.

## Package layout

```
cmd/
  q8s/            Single binary: serve / install / uninstall / status

internal/
  server/
    server.go     HTTP server, TLS setup, route registration
    handler.go    All resource handlers (Pod, Service, PVC, ConfigMap, Secret, Deployment, Job, CronJob)
    discovery.go  API discovery endpoints (/api, /apis, /version, /healthz)
    auth.go       mTLS client certificate middleware
    table.go      Table response rendering for kubectl get

  store/
    store.go      In-memory resource store with JSON persistence

  quadlet/
    generator.go  Quadlet file generators (Container, JobContainer, CronContainer, CronTimer, Volume, Network)

  systemd/
    manager.go    D-Bus Manager: start/stop/enable/disable/reload units
    activation.go Socket activation (LISTEN_FDS) and default listeners

  podman/
    podman.go     Thin wrapper around `podman ps --format json`

pkg/
  install/
    install.go    One-shot installer: cert generation, systemd unit creation
```

## Sync loop

A background goroutine polls systemd every 5 seconds and updates pod and job phases in the store:

| systemd `ActiveState` | Pod phase | Job status |
|---|---|---|
| `active` | Running | active=1 |
| `inactive` + result `success` | Succeeded | succeeded=1 |
| `inactive` + result `exit-code/signal/…` | Failed | failed=1 |
| `failed` | Failed | failed=1 |
| anything else | Pending | — |

Existing Podman containers with `io.kubernetes.pod.name` / `io.kubernetes.pod.namespace` labels are imported into the store on startup so they survive a server restart without re-applying manifests.
