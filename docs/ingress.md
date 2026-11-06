# Ingress (Traefik integration)

q8s generates [Traefik file-provider](https://doc.traefik.io/traefik/providers/file/) dynamic config files when an Ingress is created, updated, or deleted. Traefik watches the directory and hot-reloads — no restart needed.

## Requirements

1. **Traefik** attached to every namespace podman network (`q8s-{ns}`), so it can resolve and dial pods by their cluster DNS name
2. Nothing published on the host for the common case: Traefik dials `<pod>.<ns>:<containerPort>` directly inside the namespace network. An explicit `hostPort` is still honored as a host-level publish and dialed on its host address.

For `Middleware` and `IngressRouteTCP` objects, see
[Traefik Middleware and IngressRouteTCP](traefik.md).

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
# Create a deployment with a container port (no host publish needed)
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
internet → traefik (in each q8s namespace network, :80/:443) → <pod>.<ns>:<containerPort>
```

1. q8s renders every pod with DNS aliases on its namespace network: `<svc>`, `<svc>.<ns>`, `<svc>.<ns>.svc.cluster.local` and (always) `<pod>.<ns>`. Deployment replicas are ordinary pods (`web-0`, `web-1`, ...), so each one is individually addressable with no host publish and no port allocation.
2. `kubectl create ingress` → q8s writes `{dataDir}/traefik/{ns}-{name}.yaml`
3. Traefik's file provider detects the new file, applies the routing
4. `kubectl delete ingress` → q8s removes the file, traefik drops the route

## Networking

Traefik sits IN each namespace's podman network and dials backends by DNS name on the kernel path (no pasta, no host publish). An explicit `hostPort` (optionally with `hostIP`) is still a host-level publish and is dialed on that host address, so hand-published backends keep working. A `hostNetwork` pod without a `hostPort` publishes nothing addressable and is skipped.

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
| `traefik.ingress.kubernetes.io/router.middlewares` | comma-separated refs | `middlewares:` list, in the given order; a `@kubernetescrd` suffix becomes `@file` |
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

q8s can define the referenced middleware itself when it is a `Middleware`
object, or reference any middleware that already exists in Traefik's dynamic
configuration directory (the same directory q8s writes its Ingress files into:
`~/.local/share/q8s/traefik/`, or `/etc/q8s/traefik/` for rootful q8s).

Traefik's Kubernetes provider names a Middleware reference
`<namespace>-<name>@kubernetescrd`. Because q8s serves `Middleware` objects
through Traefik's file provider, q8s rewrites that provider suffix to `@file`:

```yaml
metadata:
  annotations:
    traefik.ingress.kubernetes.io/router.middlewares: default-www-redirect@kubernetescrd,default-hsts@kubernetescrd
```

renders as:

```yaml
      middlewares: ["default-www-redirect@file", "default-hsts@file"]
```

The referenced objects can then be ordinary `Middleware` resources in the
same manifest set (see [Traefik Middleware and IngressRouteTCP](traefik.md)):

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: www-redirect
  namespace: default
spec:
  redirectRegex:
    regex: "^https://example\\.com/(.*)"
    replacement: "https://www.example.com/${1}"
    permanent: true
---
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: hsts
  namespace: default
spec:
  headers:
    stsSeconds: 31536000
    stsIncludeSubdomains: true
```

A reference without a provider, and a reference with any other provider such
as `@file`, is passed through unchanged.

Annotation values are validated when the Ingress is created or updated: an
entrypoint or certresolver name must match `[A-Za-z0-9][A-Za-z0-9._-]*`, a
middleware reference must be `name` or `name@provider` (provider
`[a-z0-9][a-z0-9-]*`), `router.tls` must parse as a boolean, and
`router.priority` must be a non-negative integer. An invalid value is rejected
with HTTP 400 rather than written into the config file Traefik loads.

## Port resolution (selector-based, like Endpoints)

When the referenced Service exists, q8s resolves backends the way real k8s
builds Endpoints: the Service's **selector** picks the pods, and every
matching pod contributes **one Traefik server** for the Service's
`targetPort` (numeric or a container-port name). A pod with an explicit
`hostPort` is dialed on its host address; every other non-hostNetwork pod is
dialed at `http://<pod>.<ns>:<containerPort>`. Traefik load-balances
(round-robin) across them.

```yaml
services:
  default-web-0:
    loadBalancer:
      servers:
        - url: "http://web-0.default:8080"   # Deployment replica, no hostPort
        - url: "http://web-1.default:8080"
        - url: "http://127.0.0.1:20001"      # standalone pod with hostPort 20001
```

Key points:

- Pods behind one Service **do not need the same port** — only matching
  labels and a container port matching `targetPort`.
- A `hostNetwork` pod with no `hostPort` is skipped: it publishes nothing
  addressable. Every other pod resolves by DNS.
- The config is regenerated whenever matching pods are created or deleted
  (including via Deployment scale), so the server list tracks reality the
  way Endpoints do.
- **Deployments scale out of the box**: each replica is an ordinary pod with
  its own `<pod>.<ns>` DNS name, so scale up/down just extends or shrinks the
  server list. Nothing is published on the host and no replica ports are
  allocated.
- If the Service doesn't exist (or no matching pod yields an address), q8s
  falls back to a single `localhost:{ingress port}` backend.
