# Ingress (Traefik integration)

q8s generates [Traefik file-provider](https://doc.traefik.io/traefik/providers/file/) dynamic config files when an Ingress is created, updated, or deleted. Traefik watches the directory and hot-reloads — no restart needed.

## Requirements

1. **Traefik** running on the host (system service, container with `--network host`, or any setup that can reach `localhost`)
2. **Published ports** on your deployments — containers must publish their port to the host (`--port` flag on `kubectl create deployment`), otherwise traefik can't reach them via localhost

## Traefik static config

Add the file provider to your Traefik static configuration (`/etc/traefik/traefik.yaml` or equivalent):

```yaml
entryPoints:
  web:
    address: ":80"
  websecure:
    address: ":443"

providers:
  file:
    directory: /home/youruser/.local/share/q8s/traefik/
    watch: true
```

That's the only traefik-side configuration needed. `watch: true` means traefik picks up changes immediately when q8s writes/removes files.

For rootful q8s the directory is `/etc/q8s/traefik/`.

## Usage

```sh
# Create a deployment with a published port
kubectl create deployment myapp --image=myimage --port=8080

# Create an ingress routing to it
kubectl create ingress myapp --rule="app.example.com/*=myapp:8080"

# With TLS (see "TLS termination" below)
kubectl create ingress myapp --rule="app.example.com/*=myapp:8080,tls"

# Multiple hosts/paths
kubectl create ingress multi \
  --rule="api.example.com/v1*=api-svc:9090" \
  --rule="web.example.com/*=frontend:3000,tls"
```

## Generated config

For `kubectl create ingress myapp --rule="app.example.com/api*=backend:8080,tls"`:

```yaml
http:
  routers:
    default-myapp-0:
      rule: "Host(`app.example.com`) && PathPrefix(`/api`)"
      service: default-myapp-0
      tls: {}
  services:
    default-myapp-0:
      loadBalancer:
        servers:
          - url: "http://localhost:8080"
```

File location: `~/.local/share/q8s/traefik/{namespace}-{name}.yaml`

## How it works

```
internet → traefik (host, :80/:443) → localhost:{published_port} → podman container
```

1. Deployment replicas publish per-instance loopback ports (auto-allocated, → `PublishPort=127.0.0.1:...` in quadlets); standalone pods publish their own `hostPort`
2. `kubectl create ingress` → q8s writes `{dataDir}/traefik/{ns}-{name}.yaml`
3. Traefik's file provider detects the new file, applies the routing
4. `kubectl delete ingress` → q8s removes the file, traefik drops the route

## Networking

Traefik must be able to reach `localhost:{port}` where the container publishes. This works when traefik runs:
- As a system service directly on the host
- In a container with `--network host`
- In any setup where `localhost` resolves to the same network namespace as the published ports

Containers that **don't** publish ports (internal-only on the `q8s-{ns}` podman network) are not reachable from traefik via localhost.

## TLS termination

TLS is **Traefik's job, not q8s's**. Setting `tls` on an ingress rule makes q8s
emit `tls: {}` in the router — Traefik serves it with *its default
certificate* (self-signed unless you configured one).

`spec.tls[].secretName` is **accepted for manifest compatibility and
deliberately not read**. q8s does not participate in public-TLS lifecycles
(certificate renewal, rotation, ACME, cert-manager integration) — that would
make q8s part of the trust chain for external traffic, which is exactly the
surface q8s tries not to have. For real certificates configure Traefik
itself: `tls.certificates` in its static/dynamic config, or an ACME
`certificatesResolver` (Let's Encrypt). Containers that must own their certs
(a Caddy sidecar, certbot) can terminate TLS themselves.

## Port resolution (selector-based, like Endpoints)

When the referenced Service exists, q8s resolves backends the way real k8s
builds Endpoints: the Service's **selector** picks the pods, and every
matching pod contributes **one Traefik server** built from its own published
`hostPort` for the Service's `targetPort` (numeric or a container-port
name). Traefik load-balances (round-robin) across them.

```yaml
services:
  default-web-0:
    loadBalancer:
      servers:
        - url: "http://127.0.0.1:20001"   # pod web-a (hostPort 20001)
        - url: "http://127.0.0.1:20002"   # pod web-b (hostPort 20002)
        - url: "http://127.0.0.1:20003"   # pod web-c (hostPort 20003)
```

Key points:

- Pods behind one Service **do not need the same port** — only matching
  labels and a container port matching `targetPort`.
- Pods without a `hostPort` are skipped: with rootless podman, containers
  are only reachable from the host through published ports.
- The config is regenerated whenever matching pods are created or deleted
  (including via Deployment scale), so the server list tracks reality the
  way Endpoints do.
- **Deployments scale out of the box**: q8s allocates each replica its own
  loopback host port (range 20000–32767, bound to 127.0.0.1 only, stable
  across restarts, freed on scale-down) and publishes it automatically —
  replicas need no `hostPort` in the template. Scale up/down and the Traefik
  server list follows, one URL per replica. This is q8s's kube-proxy:
  Traefik fans requests out to the replicas.
- Keep manually-chosen `hostPorts` outside 20000–32767 to avoid colliding
  with the allocation range.
- If the Service doesn't exist (or no matching pod publishes a port), q8s
  falls back to a single `localhost:{ingress port}` backend.
