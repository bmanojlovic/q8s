# q8s

A Kubernetes-compatible API server that runs containers on a single Linux machine using Podman and systemd Quadlets. Use `kubectl` normally — but everything runs locally, no cluster required.

```
kubectl  ─── HTTPS/mTLS ──▶  q8s  ──▶  Podman Quadlets + systemd
```

## What works

- `kubectl get nodes` — real hostname, IP, OS, CPU/memory/disk, podman version
- `kubectl top nodes` / `kubectl top pods` — live CPU and memory usage
- `kubectl describe node` — full output with conditions, capacity, lease
- Pods, Deployments (scale, rollout restart, env), Services, ConfigMaps, Secrets, Jobs, CronJobs
- `kubectl logs`, `kubectl exec -it`, `kubectl get -w` (watch)
- CrashLoopBackOff detection, restart limits, delete cascade
- Namespace isolation via Podman networks
- Socket-activated — zero resource usage until first request

## Install

Download the binary from [Releases](https://github.com/bmanojlovic/q8s/releases):

```sh
curl -Lo q8s https://github.com/bmanojlovic/q8s/releases/latest/download/q8s-linux-amd64
chmod +x q8s
./q8s install
```

This generates TLS certificates, installs systemd units, and prints the exact `kubectl config` commands to run.

Alternatively, export a standalone kubeconfig file:

```sh
q8s kubeconfig > ~/.kube/q8s.yaml
export KUBECONFIG=~/.kube/config:~/.kube/q8s.yaml
```

## Start

```sh
systemctl --user enable --now q8s.socket
```

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

## Build from source

```sh
make          # vet + test + build
make install  # build + q8s install
```

Requires Go 1.26.4+.

## Uninstall

```sh
q8s uninstall
```

## System impact

- ~22 MB RSS, 0% CPU when idle
- Socket-activated: not running until first kubectl request
- Single static binary, no dependencies beyond Podman and systemd
