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

The `hostpath` class reads the host directory from the `q8s.io/host-path` annotation on the PVC. No `.volume` file is generated for hostpath PVCs.

## Port semantics

| Mechanism | Effect | Quadlet output |
|---|---|---|
| `containerPort` | Internal only — reachable within the `q8s-{ns}` podman network via NetworkAlias | None |
| `hostPort` | Binds to the host | `PublishPort=hostPort:containerPort/proto` |
| `hostPort` + `hostIP` | Binds one interface (e.g. loopback only) | `PublishPort=hostIP:hostPort:containerPort/proto` |
| Deployment replicas | Auto-allocated loopback port per replica | `PublishPort=127.0.0.1:20000-32767:containerPort` |
| Service `type: NodePort` | Binds `0.0.0.0:nodePort` on the single backing pod | `PublishPort=nodePort:targetPort/proto` |

A **ClusterIP** Service (the default) never binds a host port. It is a **selector + port map**: its name becomes a DNS alias on the namespace network (aardvark), and its selector + `targetPort` drive ingress backend resolution (one Traefik server per matching pod). For host reachability use `hostPort`, a Deployment (auto-allocated, via Ingress), or a `type: NodePort` Service.

A **NodePort** Service is a real host-level listener: q8s allocates a port in **30000–32767** (or honors an explicit in-range `spec.ports[].nodePort`) and publishes it on `0.0.0.0:nodePort → targetPort` on the Service's single backing pod. It is dumb L4 (protocol-blind, the same `podman -p` mechanism as `hostPort`) — **not** load-balanced. Because one host socket can be bound by only one process, a NodePort has exactly one backing listener: for a Deployment only instance-0 carries it (extra replicas keep their loopback port and stay Ingress-reachable). For load-balanced multi-replica exposure use an Ingress. q8s rejects a NodePort whose number is out of range or already claimed, and rejects any Service whose port collides with a matching pod's `hostPort` — those double-bind and can't work.

## Resource limits and cgroup delegation

q8s emits `Memory=` and `PodmanArgs=--cpus=N` in quadlet files when `resources.limits` are set. On cgroup v2, this requires the memory and cpu controllers to be delegated to the user session.

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
