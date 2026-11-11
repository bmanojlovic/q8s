# Configuration

## Port

The TCP port defaults to `6443`. To use a different one:

```sh
Q8S_PORT=8443 q8s install
```

The port is baked into the installed `q8s.socket` unit. Re-running `q8s install` without `Q8S_PORT` preserves the existing port.

## kubectl setup

After `q8s install` prints the commands, run them:

```sh
kubectl config set-cluster q8s \
  --server=https://localhost:6443 \
  --certificate-authority=~/.local/share/q8s/certs/ca.crt \
  --embed-certs=true

kubectl config set-credentials q8s \
  --client-certificate=~/.local/share/q8s/certs/client.crt \
  --client-key=~/.local/share/q8s/certs/client.key \
  --embed-certs=true

kubectl config set-context q8s --cluster=q8s --user=q8s
kubectl config use-context q8s
```

## Rootless vs rootful

| | Rootless | Rootful |
|---|---|---|
| Data dir | `~/.local/share/q8s` | `/etc/q8s` |
| Quadlet dir | `~/.config/containers/systemd/` | `/etc/containers/systemd/` |
| Systemd | `systemctl --user` | `systemctl` |
| Runtime | `$XDG_RUNTIME_DIR/q8s/` | `/run/q8s/` |
| Container IPs | private to the user netns (backends via published hostPorts) | routable from the host |

Rootless needs **lingering** so the systemd user manager (and with it every
q8s unit and container) keeps running after you log out:

```sh
q8s install          # enables linger automatically (falls back to a warning)
# or manually:
sudo loginctl enable-linger $USER
```

`q8s status` warns when lingering is off.

Rootful on a host with firewalld: `q8s install` also enables
`netavark-firewalld-reload.service`. firewalld drops netavark's runtime bridge
rules on `firewall-cmd --reload`, which cuts pod DNS and ingress until
`podman network reload --all` is run; the reload service listens for the
firewalld `Reloaded` signal and puts the rules back. If the unit is not
installed, install prints a warning instead of failing, and `q8s doctor` says
so. Rootless networks live inside the rootless network namespace and are not
affected.

## Commands

```sh
q8s install    # generate certs, create dirs, install systemd units
q8s install --server https://myhost:6443  # set server URL for kubeconfig
q8s install --san-ip 10.0.0.5 --san-dns myhost  # add cert SANs (persisted)
q8s install --regenerate-certs  # regen certs keeping persisted SANs
q8s uninstall  # remove systemd units
q8s serve      # run API server directly (no systemd)
q8s start      # start q8s.socket (begin accepting connections)
q8s stop       # stop socket and service
q8s enable     # enable and start socket on boot
q8s disable    # disable socket
q8s status     # show socket/service state
q8s doctor     # read-only host capability report
q8s kubeconfig # print kubeconfig to stdout (uses persisted server URL)
```

## Persistent configuration

`q8s install` writes `{dataDir}/config.json` with install-time settings:

```json
{
  "port": 6443,
  "serverURL": "https://myhost:6443",
  "extraSANIPs": ["10.0.0.5"],
  "extraSANDNS": ["myhost"]
}
```

- **Port** and **serverURL** are used by `q8s kubeconfig` and `q8s status`
- **SANs** survive cert regeneration — no need to re-pass `--san-ip`/`--san-dns`
- Port can be overridden per-invocation with `Q8S_PORT` env var
- **Idempotency**: re-running `q8s install` or `q8s kubeconfig` with unchanged
  settings does not mint a fresh CA/client identity — certs regenerate only
  when the SAN list changes or `--regenerate-certs` is passed. Repeated
  `q8s kubeconfig` fetches are byte-identical and safe to merge.

## Pod-to-Quadlet mapping

A Pod spec translates to a `.container` file:

```ini
[Container]
Image=nginx:latest
ContainerName=default-nginx-0
Network=q8s-default.network
Label=io.kubernetes.pod.name=nginx-0
Label=io.kubernetes.pod.namespace=default
Label=io.kubernetes.pod.deployment=nginx
Label=app=nginx
Environment=KEY=value
Volume=default-my-pvc.volume:/data:Z
Volume=/run/q8s/configmaps/default/config:/etc/config:ro,z

[Unit]
Description=Pod nginx-0
StartLimitBurst=5
StartLimitIntervalSec=60

[Service]
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

## Storage classes

PVCs can specify a `storageClassName` to control how volumes are mounted:

| storageClassName | Quadlet Volume= | SELinux |
|---|---|---|
| `standard` (default) | `ns-name.volume:/mount:Z` | Exclusive relabel |
| `standard-shared` | `ns-name.volume:/mount:z` | Shared relabel |
| `hostpath` | `/host/path:/mount:Z` | Exclusive relabel |

The `hostpath` class reads the host directory from the `q8s.io/host-path` annotation on the PVC. No `.volume` file is generated for hostpath PVCs. An optional `q8s.io/selinux-label` annotation selects the relabel mode for the host directory: `private` (default) emits `:Z`, `shared` emits `:z`, and any other value is rejected.

## Port semantics

| Mechanism | Effect | Quadlet output |
|---|---|---|
| `containerPort` | Internal only — reachable within the `q8s-{ns}` podman network via NetworkAlias | None |
| `hostPort` | Binds to the host | `PublishPort=hostPort:containerPort/proto` |
| `hostPort` + `hostIP` | Binds one interface (e.g. loopback only) | `PublishPort=hostIP:hostPort:containerPort/proto` |
| Deployment replicas | Addressed by cluster DNS name on the namespace network | None (no host publish) |
| Service `type: NodePort` | Binds `0.0.0.0:nodePort` on the single backing pod | `PublishPort=nodePort:targetPort/proto` |

A **ClusterIP** Service (the default) never binds a host port. It is a **selector + port map**: its name becomes a DNS alias on the namespace network (aardvark), and its selector + `targetPort` drive ingress backend resolution (one Traefik server per matching pod, by `<pod>.<ns>` DNS unless the pod declares a `hostPort`). For host reachability use `hostPort` or a `type: NodePort` Service.

A **NodePort** Service is a real host-level listener: q8s allocates a port in **30000–32767** (or honors an explicit in-range `spec.ports[].nodePort`) and publishes it on `0.0.0.0:nodePort → targetPort` on the Service's single backing pod. It is dumb L4 (protocol-blind, the same `podman -p` mechanism as `hostPort`), **not** load-balanced. Because one host socket can be bound by only one process, a NodePort has exactly one backing listener: for a Deployment only instance-0 carries it (extra replicas stay Ingress-reachable by DNS). For load-balanced multi-replica exposure use an Ingress. q8s rejects a NodePort whose number is out of range or already claimed, and rejects any Service whose port collides with a matching pod's `hostPort`, because those double-bind and can't work.

## Resource limits and cgroup delegation

q8s emits `PodmanArgs=--memory=N --memory-swap=-1` and `PodmanArgs=--cpus=N` in quadlet files when `resources.limits` are set. On cgroup v2, this requires the memory and cpu controllers to be delegated to the user session.

(Memory goes through `PodmanArgs` rather than the native quadlet `Memory=` key so it works on older podman whose quadlet generator rejects that key — same runtime effect, portable across versions.)

q8s detects whether these controllers are available at startup. If not, resource limits are **silently skipped** — the pod starts without limits rather than crashing.

To enable resource limits in rootless mode:

```sh
sudo mkdir -p /etc/systemd/system/user@.service.d
sudo tee /etc/systemd/system/user@.service.d/delegate.conf <<CONF
[Service]
Delegate=memory cpu pids
CONF
sudo systemctl daemon-reload
# re-login for the delegation to take effect
```

## GPU, shared memory and ulimits

`resources.limits["nvidia.com/gpu"]` with any value >= 1 emits `AddDevice=nvidia.com/gpu=all` for the first container. q8s does not try to count or allocate individual GPUs: podman resolves the CDI device name to every GPU the host exposes.

An `emptyDir` volume with `medium: Memory` mounted at `/dev/shm` emits `ShmSize=<sizeLimit in bytes>` and no `Volume=` line for that mount. Without a `sizeLimit` it emits nothing. Other `emptyDir` volumes keep the normal named `Volume=` line.

A pod annotation, or a Deployment/Job/CronJob pod-template annotation, `q8s.io/ulimits` holds a comma separated list such as `memlock=-1:-1,nofile=65536`. Each item becomes one `Ulimit=` line. Items must look like `name=soft` or `name=soft:hard`, otherwise the resource is rejected.

## Health checks (probes)

Podman has one health check per container, so q8s maps the Kubernetes probes onto it:

1. `livenessProbe` when set. It drives restarts: the unit gets `HealthOnFailure=kill` and the container's `Restart=` policy (`Restart=always` for `RestartPolicy: Always`) brings it back, which is podman's documented restart pattern. Without `HealthOnFailure` podman only marks the container unhealthy and never restarts it.
2. Otherwise `readinessProbe`, rendered as a status-only check with no `HealthOnFailure`.
3. Otherwise no health check.

When both are set, liveness is rendered and readiness is not represented: podman cannot carry a second independent check, so `Ready` follows the liveness check. That combination is accepted rather than rejected.

Both handlers render the command in podman's JSON exec form, written verbatim as the `HealthCmd=` value (quadlet reads the array itself and passes it to podman's `--health-cmd`):

| Probe handler | Quadlet output |
|---|---|
| `exec` | `HealthCmd=[<command...>]` |
| `httpGet` | `HealthCmd=["sh","-c",<script>,"q8s-probe",<insecure>,<url>,<headers...>]` |

In every health command, a literal `$` is written as `$$` and a literal `%` as `%%`, so systemd does not expand variables or specifiers on the host before podman hands the command to the container.

- The array carries no `"CMD"` keyword. podman 5.4 mishandles a leading `"CMD"` (it keeps the whole JSON text as one string or re-splits the arguments on whitespace, so every check fails); without it podman 5.4 and 6 both store the array as `["CMD", <command...>]`. A one-element command is run through `/bin/sh` by podman (`CMD-SHELL`), so that image needs a shell.
- `exec` keeps argument boundaries: `["sh","-c","wget -qO- http://x"]` stays one argument list instead of being joined with spaces. An empty command is an error.
- `httpGet` runs a POSIX `sh` script inside the container that uses the first HTTP client the image provides: `curl`, else `wget`, else `python3`. Each `httpHeaders` entry is passed as a separate `Name: value` argument. `scheme: HTTPS` sets the insecure flag: curl gets `-k`, wget gets `--no-check-certificate`, and python3 uses an unverified SSL context (the probe does not verify certificates, matching Kubernetes' default). `127.0.0.1` is used unless `host` is set to an IP literal; a hostname is an error because q8s does not resolve names in a probe. An empty `path` becomes `/`. Header names must be HTTP tokens and values may not contain CR, LF or NUL. The image needs `/bin/sh`; if it has none of curl, wget or python3, the check fails with a clear message. Distroless images (no shell) cannot use an httpGet probe, so use an `exec` probe there. The script sets no timeout of its own; `HealthTimeout=` bounds the check. All variable data (URL, headers and the insecure flag) is passed as positional arguments, never spliced into the script text.
- `tcpSocket` and `grpc` are rejected (422 at the API) instead of being ignored; use an `exec` probe instead.
- A named httpGet port is resolved against the container's ports; an unknown name is an error.

Timing, with the Kubernetes defaults applied when a field is 0:

| Quadlet key | Kubernetes field | Default |
|---|---|---|
| `HealthInterval=` | `periodSeconds` | 10s |
| `HealthTimeout=` | `timeoutSeconds` | 1s |
| `HealthRetries=` | `failureThreshold` | 3 |
| `HealthStartPeriod=` | `initialDelaySeconds` | omitted when 0 |

A `startupProbe` becomes podman's startup health check: `HealthStartupCmd=` (same command rendering), `HealthStartupInterval=`, `HealthStartupTimeout=`, `HealthStartupRetries=` (from `failureThreshold`, default 3) and `HealthStartupSuccess=` (from `successThreshold`, default 1). Podman requires a main health check for it, so a `startupProbe` without a `livenessProbe` or `readinessProbe` is an error.

All of these rules are enforced at the API on Pod and Deployment create, update and patch (and on Job and CronJob create/patch, which render health checks too), so an unsupported probe returns 422 instead of being silently dropped.

## CronJob timer translation

| Cron expression | systemd OnCalendar= |
|---|---|
| `0 3 * * *` | `*-*-* 3:0:00` |
| `*/5 * * * *` | `*-*-* *:0/5:00` |
| `0 0 1 * *` | `*-*-1 0:0:00` |

## Testing

Run the smoke test suite against a running q8s instance:

```sh
pip install pyyaml
python3 tests/smoke/runner.py
```

Or the full e2e (starts its own server):

```sh
uv run e2e_test.py
```
