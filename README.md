# q8s

A Kubernetes-compatible API server that runs containers on a single Linux machine using Podman and systemd Quadlets. Use `kubectl` normally — but everything runs locally, no cluster required.

```
kubectl  ─── HTTPS/mTLS ──▶  q8s  ──▶  Podman Quadlets + systemd
```

## What works

- `kubectl get nodes` — real hostname, IP, OS, CPU/memory/disk, podman version
- `kubectl top nodes` / `kubectl top pods` — live CPU and memory usage
- `kubectl describe node` — full output with conditions, capacity, lease
- Pods, Deployments (scale, rollout restart, env), Services, ConfigMaps, Secrets, Jobs, CronJobs, Namespaces
- PersistentVolumeClaims backed by Podman volumes or host directories ([storage classes](docs/configuration.md#storage-classes))
- Ingress, plus Traefik `Middleware` and `IngressRouteTCP` (`traefik.io/v1alpha1`); see [docs/ingress.md](docs/ingress.md) and [docs/traefik.md](docs/traefik.md) (run Traefik v3.7.17 or newer)
- `kubectl logs`, `kubectl exec -it`, `kubectl get -w` (watch), `kubectl apply --server-side`
- OpenAPI schema for clients that need one, e.g. terraform's `kubernetes_manifest` ([docs/openapi.md](docs/openapi.md))
- CrashLoopBackOff detection, restart limits, delete cascade
- Namespace isolation via Podman networks (`q8s-<namespace>`)
- Rootless (per user) or rootful (system-wide); rootful pods run in their own user namespace ([details](docs/architecture.md#user-namespaces-rootful))
- Socket-activated: nothing runs until the first request

Not supported: StatefulSets, DaemonSets and ReplicaSets are listed so `kubectl get all` works, but are always empty and read-only.

## Install

Download the binary from [Releases](https://github.com/bmanojlovic/q8s/releases):

```sh
curl -Lo q8s https://github.com/bmanojlovic/q8s/releases/latest/download/q8s-linux-amd64
chmod +x q8s
./q8s install
```

This generates TLS certificates, installs systemd units, and prints the exact `kubectl config` commands to run.
Run it as root (`sudo ./q8s install`) for a system-wide install: data in `/etc/q8s`, units in `/etc/systemd/system`.
Rootless needs lingering, so the user's systemd manager and every pod keep running after logout;
`q8s install` enables it (or prints the `sudo loginctl enable-linger` command), and `q8s status` warns when it is off.
See [docs/configuration.md](docs/configuration.md#rootless-vs-rootful).

Alternatively, export a standalone kubeconfig file:

```sh
(umask 077; q8s kubeconfig > ~/.kube/q8s.yaml)   # file contains the client key
export KUBECONFIG=~/.kube/config:~/.kube/q8s.yaml
```

## Start

```sh
q8s enable                                   # same as below, for either mode
systemctl --user enable --now q8s.socket     # rootless
sudo systemctl enable --now q8s.socket       # rootful
```

`q8s status`, `q8s start`, `q8s stop` and `q8s disable` manage the same units.

Or run directly:

```sh
q8s serve
```

## Verify

```sh
kubectl --context q8s get nodes
kubectl --context q8s top nodes
kubectl --context q8s create deployment --image=nginx nginx
kubectl --context q8s get pods
```

Run `q8s doctor` for a read-only report of what this host gives your workloads; see [docs/doctor.md](docs/doctor.md).

## Build from source

```sh
make          # vet + test + build
make install  # build + q8s install
```

Requires Go 1.26.4+.

The OpenAPI schema and embedded CRD list are generated and committed; see
[docs/openapi.md](docs/openapi.md) for regeneration.

## Uninstall

```sh
q8s uninstall
```

## System impact

- About 12-20 MB RSS, 0% CPU when idle
- Socket-activated: not running until the first kubectl request; after that `q8s serve` keeps running
- Pods are ordinary systemd units: they keep running when q8s is stopped
- Single static binary, no dependencies beyond Podman and systemd
