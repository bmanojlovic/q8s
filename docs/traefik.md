# Traefik Middleware and IngressRouteTCP

q8s serves a subset of Traefik's own `traefik.io/v1alpha1` resources, so the
same manifests that work on a real Kubernetes cluster with the Traefik
Kubernetes provider also work here:

- `Middleware` for the HTTP middlewares needed by web front ends: IP
  allowlists, redirects, path rewriting and security headers.
- `IngressRouteTCP` for TCP entrypoints such as mail ports, including the
  PROXY protocol and TLS passthrough.

Both kinds are stored like any other q8s object, appear in discovery under
`traefik.io/v1alpha1`, and are accepted by `kubectl`, the Python client and
Terraform's `kubernetes_manifest`.

## Traefik version

q8s does not ship Traefik; you run it as an ordinary pod. Use **Traefik v3.7.17 or
newer** (e.g. `image: traefik:v3.7.17`): it is the first release with
andybalholm/brotli v1.2.7, which fixes CVE-2026-111497. Pin an exact tag rather than `traefik:v3` or `latest`, so the version
you run is the version you checked.

## Generated files

q8s translates both kinds into [Traefik file-provider](https://doc.traefik.io/traefik/providers/file/)
configuration in the same directory as Ingress files (`TraefikDir`:
`~/.local/share/q8s/traefik/`, or `/etc/q8s/traefik/` for rootful q8s):

| Object | File | Traefik object name |
|---|---|---|
| `Middleware default/allow` | `middleware-default-allow.yaml` | `default-allow` |
| `IngressRouteTCP default/smtp` | `tcp-default-smtp.yaml` | `default-smtp-0`, `default-smtp-1`, ... |
| `Ingress default/web` | `default-web.yaml` | `default-web-0`, `default-web-1`, ... |

Files are written atomically and are left untouched when their content has
not changed, because Traefik's file watcher reloads on every write. Deleting
the object or its namespace deletes the file. Startup reconciliation writes
any file that is missing.

## Middleware

The API accepts exactly one middleware type per object. Setting zero or more
than one type is rejected with HTTP 422, as is any field outside the subset
below. That includes Traefik types q8s does not render, such as `basicAuth`,
`rateLimit` or `chain`: a manifest is either fully supported or rejected, it
is never silently pruned.

### `ipAllowList`

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: admin-allow
  namespace: default
spec:
  ipAllowList:
    sourceRange:
      - 203.0.113.7/32
      - 10.0.0.0/8
    ipStrategy:
      depth: 1
      excludedIPs:
        - 192.0.2.10
    rejectStatusCode: 403
```

- `sourceRange` entries must be an IP address or CIDR.
- `ipStrategy.depth` must be zero or greater.
- `ipStrategy.excludedIPs` entries must be an IP address or CIDR.

### `redirectRegex`

```yaml
spec:
  redirectRegex:
    regex: "^https://www\\.example\\.com/(.*)"
    replacement: "https://example.com/${1}"
    permanent: true
```

`regex` must compile with Go's `regexp` package.

### `redirectScheme`

```yaml
spec:
  redirectScheme:
    scheme: https
    port: "443"
    permanent: true
```

`scheme` must be `http` or `https`. `port`, when set, must contain digits
only.

### `headers`

```yaml
spec:
  headers:
    customRequestHeaders:
      X-Real-IP: remoteAddr
    customResponseHeaders:
      X-Frame-Options: SAMEORIGIN
    stsSeconds: 31536000
    stsIncludeSubdomains: true
    stsPreload: true
    forceSTSHeader: true
    frameDeny: true
    contentTypeNosniff: true
    browserXssFilter: true
    referrerPolicy: no-referrer
    contentSecurityPolicy: "default-src 'self'"
    permissionsPolicy: "geolocation=()"
    customFrameOptionsValue: SAMEORIGIN
```

Custom header names must be valid HTTP token characters. Values may not
contain CR, LF or NUL. `stsSeconds` must not be negative.

### `replacePath`

```yaml
spec:
  replacePath:
    path: /new/path
```

`path` is required and must start with `/`.

### `replacePathRegex`

```yaml
spec:
  replacePathRegex:
    regex: "^/old/(.*)"
    replacement: "/new/${1}"
```

`regex` is required and must compile with Go's `regexp` package.
`replacement` may be empty.

### `stripPrefix`

```yaml
spec:
  stripPrefix:
    prefixes:
      - /api
      - /v1
```

`prefixes` must contain at least one entry. Every entry must be non-empty and
start with `/`. `forceSlash` is deprecated in Traefik v3 and is not accepted.

### `stripPrefixRegex`

```yaml
spec:
  stripPrefixRegex:
    regex:
      - "^/api/v[0-9]+"
      - "^/legacy"
```

`regex` must contain at least one entry. Every entry must be non-empty and
compile with Go's `regexp` package.

### `addPrefix`

```yaml
spec:
  addPrefix:
    prefix: /base
```

`prefix` is required and must start with `/`.

Path and regex fields may not contain control characters (anything below
0x20 or 0x7f).

## Ingress middleware references

Traefik's Kubernetes provider names middlewares `<namespace>-<name>@kubernetescrd`.
q8s serves `Middleware` objects through Traefik's file provider, so an
Ingress annotation reference ending in `@kubernetescrd` is rewritten to
`@file`:

```yaml
metadata:
  annotations:
    traefik.ingress.kubernetes.io/router.middlewares: default-admin-allow@kubernetescrd
```

renders as:

```yaml
      middlewares: ["default-admin-allow@file"]
```

References without a provider, or with another provider such as an existing
`@file` reference, are passed through unchanged. This means an Ingress
manifest written for real Kubernetes + Traefik works unchanged, and the
referenced `Middleware` can be created by the same manifest set. See
[Ingress](ingress.md) for the rest of the router annotations.

## IngressRouteTCP

```yaml
apiVersion: traefik.io/v1alpha1
kind: IngressRouteTCP
metadata:
  name: smtp
  namespace: default
spec:
  entryPoints:
    - smtp
  routes:
    - match: HostSNI(`*`)
      services:
        - name: stalwart
          port: 25
          proxyProtocol:
            version: 2
  tls:
    passthrough: true
```

Supported subset:

- `entryPoints` is required and must contain valid Traefik entrypoint names
  (`^[A-Za-z0-9][A-Za-z0-9._-]*$`).
- `routes` must contain at least one route. `match` must be exactly
  ``HostSNI(`*`)``; other matchers are rejected with HTTP 422 because q8s
  only renders the catch-all TCP router.
- Each route must have exactly one `services` entry. `name` is a Service in
  the same namespace. `port` is the Service port number or its name.
  `proxyProtocol.version` is optional and must be 1 or 2.
- `tls` is optional and may only contain `passthrough`. Any other TLS field
  is rejected.

A catch-all `HostSNI(`*`)` router owns its entrypoint. Creating or updating
an `IngressRouteTCP` that reuses an entrypoint already used by another
`IngressRouteTCP` in any namespace returns HTTP 409 Conflict.

The file layout for the example above is:

```yaml
tcp:
  routers:
    default-smtp-0:
      entryPoints:
      - smtp
      rule: HostSNI(`*`)
      service: default-smtp-0
      tls:
        passthrough: true
  serversTransports:
    default-smtp-0:
      proxyProtocol:
        version: 2
  services:
    default-smtp-0:
      loadBalancer:
        servers:
        - address: mail-0.default:25
        serversTransport: default-smtp-0
```

`proxyProtocol` on the IngressRouteTCP service is rendered as a TCP
`serversTransport` of the same name, which the load balancer references.
Traefik v3 deprecates `proxyProtocol` directly on the load balancer.

Backends are resolved exactly like Ingress backends: the Service selector
selects the pods, the Service port resolves to its `targetPort`, and every
non-terminal matching pod contributes one server. A pod that is not
`hostNetwork` is dialed at `<pod>.<namespace>:<containerPort>`. A
`hostNetwork` pod is dialed on its `hostPort`, and is skipped when it has no
`hostPort`. The list is regenerated when pods or Services change.

## Entrypoints must exist in Traefik's static config

Traefik entrypoints are static configuration; q8s writes routers that
reference them but does not create listeners. An `IngressRouteTCP` entrypoint
such as `smtp` only works if Traefik's static configuration defines it:

```yaml
entryPoints:
  web:
    address: ":80"
  websecure:
    address: ":443"
  smtp:
    address: ":25"
  submission:
    address: ":587"
  imaps:
    address: ":993"
```

For a rootless, socket-activated Traefik, q8s can pass the listening sockets
to the Traefik pod instead of publishing every port on the host. Annotate the
Traefik pod (or Deployment template) with `q8s.io/socket-activation`:

```yaml
metadata:
  annotations:
    q8s.io/socket-activation: "web=80,websecure=443,smtp=25,submission=587,imaps=993"
```

systemd then owns the sockets and passes them to Traefik, so Traefik needs no
`hostPort` for those entrypoints. The entrypoints must still be named the
same way in Traefik's static configuration.

## Terraform

`kubernetes_manifest` can create both kinds. The read-only CRD API serves
their upstream schemas, and apply works through the normal create and patch
paths.

```hcl
terraform {
  required_providers {
    kubernetes = { source = "hashicorp/kubernetes" }
  }
}

provider "kubernetes" {
  config_path = "/tmp/q8s-tf/kubeconfig"
}

resource "kubernetes_manifest" "admin_allow" {
  manifest = {
    apiVersion = "traefik.io/v1alpha1"
    kind       = "Middleware"
    metadata = {
      name      = "admin-allow"
      namespace = "default"
    }
    spec = {
      ipAllowList = {
        sourceRange = ["203.0.113.7/32"]
      }
    }
  }
}

resource "kubernetes_manifest" "smtp" {
  manifest = {
    apiVersion = "traefik.io/v1alpha1"
    kind       = "IngressRouteTCP"
    metadata = {
      name      = "smtp"
      namespace = "default"
    }
    spec = {
      entryPoints = ["smtp"]
      routes = [{
        match = "HostSNI(`*`)"
        services = [{
          name          = "stalwart"
          port          = 25
          proxyProtocol = { version = 2 }
        }]
      }]
    }
  }
}
```
