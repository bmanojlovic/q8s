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
q8s install    # generate certs, install systemd units, copy binary to ~/.local/bin/
q8s uninstall  # remove systemd units
q8s serve      # run API server directly (no systemd)
q8s start      # start q8s.socket (begin accepting connections)
q8s stop       # stop socket and service
q8s enable     # enable and start socket (persist across reboots)
q8s disable    # disable socket
q8s status     # show socket/service state
q8s kubeconfig # print kubeconfig to stdout
```

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
PublishPort=80:80/tcp
Volume=default-my-pvc.volume:/data
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
