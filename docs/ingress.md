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
emit `tls: {}` in the router. In Traefik v3 a router-level `tls` block
**replaces** the entrypoint's `http.tls` defaults, so `tls: {}` also drops a
`certResolver` configured on the entrypoint and Traefik serves its default
self-signed certificate instead of a Let's Encrypt one. To get ACME
certificates for a specific Ingress, set the
`traefik.ingress.kubernetes.io/router.tls.certresolver` annotation (see
"Traefik annotations" below): q8s then writes
`tls: {certResolver: ...}` for the router instead of an empty block.

`spec.tls[].secretName` is **accepted for manifest compatibility and
deliberately not read**. q8s does not participate in public-TLS lifecycles
(certificate renewal, rotation, ACME, cert-manager integration); that would
make q8s part of the trust chain for external traffic, which is exactly the
surface q8s tries not to have. For real certificates configure Traefik
itself: `tls.certificates` in its static/dynamic config, an ACME
`certificatesResolver` (Let's Encrypt), or the per-Ingress annotation above.
Containers that must own their certs (a Caddy sidecar, certbot) can terminate
TLS themselves.

## Traefik annotations

q8s honors the router annotations of Traefik's own KubernetesIngress provider,
with the same keys and the same meaning, so an Ingress manifest written for
real k8s + Traefik works here unchanged. Every router generated from the
Ingress (one per rule/path) gets the same settings.

| annotation | value | router output |
|---|---|---|
| `traefik.ingress.kubernetes.io/router.entrypoints` | comma-separated names | `entryPoints:` list |
| `traefik.ingress.kubernetes.io/router.middlewares` | comma-separated refs | `middlewares:` list, in the given order |
| `traefik.ingress.kubernetes.io/router.tls` | `"true"` / `"false"` | `"true"` adds a `tls` block even without `spec.tls` |
| `traefik.ingress.kubernetes.io/router.tls.certresolver` | resolver name | `tls:` block with `certResolver:` (implies tls) |
| `traefik.ingress.kubernetes.io/router.priority` | integer | `priority:` |

List items are split on commas, trimmed of surrounding spaces, and empty items
are dropped. `router.tls: "false"` without a certresolver and without
`spec.tls` means no `tls` block. Other
`traefik.ingress.kubernetes.io/*` annotations are ignored.

Example:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
    traefik.ingress.kubernetes.io/router.middlewares: www-redirect@file,hsts@file
    traefik.ingress.kubernetes.io/router.tls.certresolver: le
    traefik.ingress.kubernetes.io/router.priority: "10"
spec:
  rules:
    - host: app.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: myapp
                port:
                  number: 8080
```

q8s renders:

```yaml
http:
  routers:
    default-myapp-0:
      rule: "Host(`app.example.com`)"
      service: default-myapp-0
      entryPoints: ["websecure"]
      middlewares: ["www-redirect@file", "hsts@file"]
      priority: 10
      tls:
        certResolver: "le"
```

q8s only **references** middlewares, it never defines them. A `@file`
middleware must exist in Traefik's dynamic configuration directory - the same
directory q8s writes its Ingress files into (`~/.local/share/q8s/traefik/`,
or `/etc/q8s/traefik/` for rootful q8s), for example:

```yaml
# ~/.local/share/q8s/traefik/middlewares.yaml
http:
  middlewares:
    www-redirect:
      redirectRegex:
        regex: "^https://example\\.com/(.*)"
        replacement: "https://www.example.com/${1}"
        permanent: true
    hsts:
      headers:
        stsSeconds: 31536000
        stsIncludeSubdomains: true
```

Annotation values are validated when the Ingress is created or updated: an
entrypoint or certresolver name must match `[A-Za-z0-9][A-Za-z0-9._-]*`, a
middleware reference must be `name` or `name@provider` (provider
`[a-z0-9][a-z0-9-]*`), `router.tls` must parse as a boolean, and
`router.priority` must be a non-negative integer. An invalid value is rejected
with HTTP 400 rather than written into the config file Traefik loads.

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
