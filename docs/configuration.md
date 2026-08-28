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
  --client-certificate=~/.local/share/q8s/certs/client.crt \
  --client-key=~/.local/share/q8s/certs/client.key \
  --embed-certs=true

kubectl config set-credentials q8s-user --embed-certs=true
kubectl config set-context q8s --cluster=q8s --user=q8s-user
kubectl config use-context q8s
```

## Rootless vs rootful

| | Rootless | Rootful |
|---|---|---|
| Data dir | `~/.local/share/q8s` | `/etc/q8s` |
| Quadlet dir | `~/.config/containers/systemd/` | `/etc/containers/systemd/` |
| Systemd | `systemctl --user` | `systemctl` |
| Runtime | `$XDG_RUNTIME_DIR/q8s/` | `/run/q8s/` |

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
| Service `.ports` | Binds to host via systemd socket unit | `{name}-{port}.socket` |

`hostPort` and Service are **mutually exclusive** on the same port. q8s rejects creation if both would bind the same host port.

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
