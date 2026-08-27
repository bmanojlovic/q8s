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

# With TLS
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

1. Deployment publishes a port to the host via `hostPort` (→ `PublishPort` in quadlet) or a Service (→ systemd `.socket` unit)
2. `kubectl create ingress` → q8s writes `{dataDir}/traefik/{ns}-{name}.yaml`
3. Traefik's file provider detects the new file, applies the routing
4. `kubectl delete ingress` → q8s removes the file, traefik drops the route

## Networking

Traefik must be able to reach `localhost:{port}` where the container publishes. This works when traefik runs:
- As a system service directly on the host
- In a container with `--network host`
- In any setup where `localhost` resolves to the same network namespace as the published ports

Containers that **don't** publish ports (internal-only on the `q8s-{ns}` podman network) are not reachable from traefik via localhost.

## Port resolution

If the referenced Service exists in the store, q8s resolves the port from the Service spec. Otherwise uses the port number from the ingress rule directly.
