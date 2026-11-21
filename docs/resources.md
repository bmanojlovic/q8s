# Supported Resources

Pods and Deployments accept one or more containers; multiple containers run as one Podman pod. Jobs and CronJobs run exactly one container; sidecars there, and `initContainers` and `ephemeralContainers` everywhere, are rejected.

A Pod, Deployment, Job or CronJob owns every Podman container, systemd unit,
EnvironmentFile and emptyDir volume name it generates. Creating one, or
updating one so that it adds a name, is refused with 409 Conflict when another
workload in any namespace already generates that name. See
[RBAC and user identities](rbac.md) for the cross-namespace rule.

## core/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `nodes` | get, list | Synthetic single node with real machine stats |
| `namespaces` | get, list, create, patch, delete | Creates a `q8s-{ns}.network` Podman network; delete stops `q8s-{ns}-network.service`, removes the `.network` Quadlet and runs `podman network rm q8s-{ns}`; patch updates labels and annotations |
| `pods` | get, list, create, patch, delete | Writes a `.container` Quadlet, or `.container` units plus a `.pod` unit for a multi-container pod; patch rewrites and restarts; also lists derived CronJob runs |
| `services` | get, list, create, patch, delete | Selector + port map: DNS aliases (NetworkAlias) and ingress backend resolution; never binds a host port |
| `persistentvolumeclaims` | get, list, create, update, patch, delete | Named Podman volume; storageClass selects mount mode |
| `configmaps` | get, list, create, update, patch, delete | Files at `{configDir}/{ns}/{name}/`; `data` and `binaryData` values, 1 MiB total |
| `secrets` | get, list, create, patch, delete | Files at `{secretDir}/{ns}/{name}/` (mode 0600); `data` and `stringData` values, 1 MiB total |
| `serviceaccounts` | get, list, create, update, patch, delete | Identity for pod credentials; no token Secrets |
| `events` | get, list, watch | In-memory, last 500, server-generated |

A ServiceAccount is only an identity: q8s stores no token Secret for it and
runs no token controller. See
[ServiceAccounts in pods](rbac.md#serviceaccounts-in-pods).

A Secret or ConfigMap change reaches the next CronJob run: q8s re-renders the
CronJob's EnvironmentFile and `.container` unit when a Secret or ConfigMap it
reads through `env[].valueFrom` or `envFrom[]` changes, without starting a run
or changing a suspended timer. Pods, Deployments and running Jobs keep their
environment until they are recreated, as in Kubernetes.

A `restartPolicy: Never` pod whose container exits quickly reaches
`Succeeded`. q8s compares the unit's `ExecMainStartTimestamp` with the pod's
creation time, so a unit that ran for this pod is not mistaken for one that
has not started yet.

## apps/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `deployments` | get, list, create, patch, delete, scale | Indexed `.container` units, plus a `.pod` unit per instance for a multi-container pod; scale, rollout restart, set env |
| `daemonsets` | get, list | Empty stub |
| `statefulsets` | get, list | Empty stub |
| `replicasets` | get, list | Empty stub |

## batch/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `jobs` | get, list, create, update, patch, delete | `Restart=no`, oneshot; pod template is immutable; also lists derived CronJob runs |
| `cronjobs` | get, list, create, update, patch, delete | `.container` + `.timer` unit; every run appears as a derived Job and Pod |

A Job's pod template is immutable: a PUT that changes `spec.template` is
rejected with 422, matching Kubernetes.

A stored Job gets `spec.selector` matching `job-name=<name>` and the
`job-name` and `batch.kubernetes.io/job-name` template labels when it is
created without them; updates keep them. One Pod named `<job name>-run` is
derived from it on read, with its phase taken from the Job's systemd unit
state (and the newest run in the journal when that is available).
`kubectl logs job/<name>` uses the job unit's output. Deleting the Job
removes the derived Pod, and writes to that Pod answer 405.

A CronJob run appears as a Job named `<cronjob>-<start unix minutes>` and a
Pod named `<job name>-run`; older runs in the same minute get a `-2`, `-3`
suffix. Nothing new is stored: both objects are derived from the systemd
journal on every request. `successfulJobsHistoryLimit` (default 3) and
`failedJobsHistoryLimit` (default 1) select how many finished runs are
listed, and running runs are always shown. `kubectl logs job/<name>` reads
the run's output from the journal, which needs Podman's journald log driver
(the default under systemd). Deleting or updating a run is not implemented
and answers 405; delete the CronJob itself instead. Runs that started before
the CronJob was created (an earlier CronJob with the same name) are not
shown. Runs are never part of a
watch response.

## rbac.authorization.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `roles` | get, list, create, update, patch, delete | Namespaced; rules need verbs and resources (`nonResourceURLs` only in a ClusterRole) |
| `rolebindings` | get, list, create, update, patch, delete | Namespaced; `roleRef` is immutable; a RoleBinding may point at a Role or a ClusterRole |
| `clusterroles` | get, list, create, update, patch, delete | `cluster-admin`, `admin`, `edit` and `view` are built-in and read-only |
| `clusterrolebindings` | get, list, create, update, patch, delete | `roleRef` must be a ClusterRole and is immutable |

Writes to these objects are administrator-only. Deleting a namespace deletes
its Roles and RoleBindings. See [RBAC and user identities](rbac.md).

## authorization.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `selfsubjectaccessreviews` | create | `kubectl auth can-i` |
| `selfsubjectrulesreviews` | create | `kubectl auth can-i --list` |
| `subjectaccessreviews` | create | Administrators only |

## authentication.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `selfsubjectreviews` | create | `kubectl auth whoami` |

## networking.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `ingresses` | get, list, create, patch, delete | Traefik dynamic config per ingress; backends resolved from the Service selector (one server per matching pod's hostPort, like Endpoints) |

## traefik.io/v1alpha1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `middlewares` | get, list, create, update, patch, delete | HTTP `ipAllowList`, `redirectRegex`, `redirectScheme`, `headers`, `replacePath`, `replacePathRegex`, `stripPrefix`, `stripPrefixRegex` and `addPrefix` subsets; one type per object, rendered as a Traefik file-provider middleware |
| `ingressroutetcps` | get, list, create, update, patch, delete | Catch-all `HostSNI(`*`)` TCP routers with optional PROXY protocol and TLS passthrough; an entrypoint may be owned by only one object |

See [Traefik Middleware and IngressRouteTCP](traefik.md) for the supported
subset, generated files and the `@kubernetescrd` to `@file` middleware
reference rewrite.

## metrics.k8s.io/v1beta1

| Resource | Notes |
|---|---|
| `nodes` | Real CPU (from /proc/stat) and memory usage |
| `pods` | Per-pod stats from podman stats |

## coordination.k8s.io/v1

| Resource | Notes |
|---|---|
| `leases` | Synthetic node lease for kubectl describe node |

## storage.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `storageclasses` | get, list | Fixed set: `standard`, `standard-shared`, `hostpath` |

### Storage classes

| Class | Volume type | SELinux | Use case |
|---|---|---|---|
| `standard` (default) | Podman named volume | `:Z` (exclusive) | Single-pod volumes |
| `standard-shared` | Podman named volume | `:z` (shared) | Multiple pods sharing a volume |
| `hostpath` | Bind mount from host | `:Z` (exclusive) | Pre-existing host directories |

PVCs bind immediately at creation (`status.phase: Bound`) — the backing
volume is a quadlet file, so there is no asynchronous provisioner to wait
on. A claim without `storageClassName` is defaulted to `standard`, matching
the `is-default-class` annotation on that class. For `standard`/`standard-shared`
the named Podman volume is created right away (the volume unit is started at
PVC creation); `hostpath` claims bind to the directory named in their
annotation.

Named volumes are namespace-scoped as `{namespace}-{name}` (also surfaced
in `spec.volumeName`), so same-named claims in different namespaces — or a
claim colliding with a volume some unrelated non-q8s deployment created —
never share one backing volume, and `podman volume ls` shows which
namespace/claim owns each volume.

Deleting a PVC that no workload uses stops the generated volume unit and
removes its `.volume` quadlet file, but keeps the Podman volume
`{namespace}-{name}` and the data in it: q8s never deletes PVC data
automatically. A namespace delete stops the namespace's workloads first and
removes every claim the same way.

A PVC still mounted by a stored Pod or a running Job is protected with the
`kubernetes.io/pvc-protection` finalizer. DELETE then only sets
`metadata.deletionTimestamp`, so `kubectl` shows the claim as Terminating,
and keeps the volume unit and its `.volume` file. The sync loop deletes the
claim as soon as the last Pod or running Job that mounts it is gone. A Pod
created while the claim is Terminating waits with `persistentvolumeclaim
"<claim>" is being deleted`, like a Pod waiting for a missing claim. Removing
the finalizer, for example with `kubectl patch pvc <claim> -p
'{"metadata":{"finalizers":[]}}' --type=merge`, deletes the claim right away,
as in Kubernetes. Remove the leftover volume manually when its data is no
longer needed: `podman volume rm {namespace}-{name}`. `q8s doctor` lists such
a leftover as `data of deleted PVC` and prints that command.

The `hostpath` class requires a `q8s.io/host-path` annotation on the PVC specifying the absolute host directory. It also honours an optional `q8s.io/selinux-label` annotation: `private` (the default, `:Z`) relabels the host directory for one container, while `shared` (`:z`) lets several containers mount it without locking each other out. Any other value is rejected.

Mounts with `readOnly: true`, or PVC volume sources with `persistentVolumeClaim.readOnly: true`, get the Podman `ro` mount option (`:ro,Z`, `:ro,z`, or `:ro`).

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  annotations:
    q8s.io/host-path: "/srv/data/myapp"
spec:
  storageClassName: hostpath
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
```

### Pod volumes

A Pod or pod template accepts these volume sources: `persistentVolumeClaim`,
`configMap`, `secret` and `emptyDir`. Any other source such as `hostPath`,
`nfs`, `projected`, `downwardAPI` or `csi` is rejected with 422. Use a
`hostpath` PersistentVolumeClaim for a host directory; a pod never mounts one
directly.

An `emptyDir` is a Podman named volume. A bare Pod uses
`q8s_{namespace}_{pod}_{volume}`. Each replica of a Deployment uses its own
volume, `q8s_{namespace}_{deployment}_{replica-index}_{volume}`, so a file
written by one replica is not visible in another, as in Kubernetes. Job and
CronJob runs keep the per-workload name
`q8s_{namespace}_{job-or-cronjob}_{volume}`. The underscore separator cannot
appear in a Kubernetes name, so a bare-pod name and a replica name can never
collide (they have four and five underscore-separated parts), and an
emptyDir can never collide with another namespace's emptyDir or with a PVC
volume, which is named `{namespace}-{claim}`. An `emptyDir` with
`medium: Memory` mounted at `/dev/shm` is rendered as `ShmSize=` instead, as
described above under Pods.

A bare Pod's emptyDir lives as long as the Pod. A Deployment replica's
emptyDir belongs to that replica incarnation: it is removed, after the
container is stopped and removed, when the replica pod is deleted through the
API, when a scale-down drops that replica, and when the Deployment or its
namespace is deleted. A container restart, including a Podman restart, keeps
the volume, as in Kubernetes. A rollout or template change keeps the volume
because the existing redeploy path restarts the unit in place rather than
stopping it and starting a new pod. Deleting a single Deployment replica pod
through the API also recreates that replica in the same request when the
Deployment still exists and its index is below `spec.replicas`; there is no
controller that would do it later. A volume that stays behind is reported by
`q8s doctor`, which prints the `podman volume rm` command to clean it up.

Only `metadata.labels` values that satisfy the Kubernetes label rule are
stored, because pod labels are copied onto the generated container as a
whitespace-separated `Label=` list.

## apiextensions.k8s.io/v1

| Resource | kubectl verbs | Notes |
|---|---|---|
| `customresourcedefinitions` | get, list | Read-only fixed list of `middlewares.traefik.io` and `ingressroutetcps.traefik.io`, generated from the Traefik CRD file |

CRDs are not stored or modified by q8s. The list exists so clients that
build their type information from CRD schemas (Terraform's
`kubernetes_manifest`, for example) can plan Traefik Middleware and
IngressRouteTCP objects.

## Pod subresources

| Subresource | kubectl command | Notes |
|---|---|---|
| `log` | `kubectl logs [-f] [--tail N] [--timestamps]` | Live containers stream `podman logs`; a finished or removed container is read from the journal (the pod's unit and `CONTAINER_NAME`), limited to lines since the pod was created |
| `exec` | `kubectl exec [-it] pod -- cmd` | WebSocket v5.channel.k8s.io |
| `attach` | `kubectl attach pod` | WebSocket attach to the container's main process |

## Features

- **Watch**: all list endpoints support `?watch=true` (kubectl get -w, Freelens, k9s)
- **Label selectors**: `key=value`, `key==value`, `key!=value`, `key`, `!key` on all list endpoints
- **Patch**: JSON merge patch and strategic merge patch (array-merge-by-name for containers/env/volumes); RFC 6902 JSON Patch (`application/json-patch+json`) on all resources; Terraform's provider patches Secret `data` keys and container/spec changes this way
- **Server-side apply**: `PATCH` with `Content-Type: application/apply-patch+yaml` is translated to a create or merge patch. q8s has no managed fields, so apply uses a single-manager model: the applier owns `spec`, `data`, `binaryData`, `stringData`, `metadata.labels` and `metadata.annotations`. `fieldManager` and `force` are accepted and ignored, and `dryRun` is rejected
- **Resource limits**: `resources.limits.memory` → `PodmanArgs=--memory=N --memory-swap=-1`, `resources.limits.cpu` → `PodmanArgs=--cpus=N` (via PodmanArgs for podman-version portability; skipped when cgroup controllers not delegated)
- **GPU**: `resources.limits["nvidia.com/gpu"]` of at least 1 → `AddDevice=nvidia.com/gpu=all` (CDI; podman resolves the GPU devices). The count is not allocated, so any request of 1 or more exposes every CDI GPU on the host.
- **Shared memory**: an `emptyDir` with `medium: Memory` mounted at `/dev/shm` → `ShmSize=<bytes>` from `sizeLimit`; no `Volume=` line is generated for it. Without a `sizeLimit`, nothing is emitted.
- **Ulimits**: pod annotation `q8s.io/ulimits` (for example `memlock=-1:-1,nofile=65536`) → one `Ulimit=<item>` line per comma separated item. Items must match `name=soft[:hard]` and are rejected otherwise.
- **Health checks**: `livenessProbe` owns the container's single podman health check (`HealthCmd=` in JSON exec form, plus `HealthOnFailure=kill` so the unit's `Restart=` actually restarts a failed container); otherwise `readinessProbe` supplies a status-only check. Both render `HealthInterval=` (from `periodSeconds`, default 10s), `HealthTimeout=` (from `timeoutSeconds`, default 1s), `HealthRetries=` (from `failureThreshold`, default 3) and `HealthStartPeriod=` (from `initialDelaySeconds`, only when set). An httpGet probe becomes a small `sh` script that requests `<scheme>://127.0.0.1:<port><path>` with the first client the image has (curl, else wget, else python3), so it needs `/bin/sh` and one of those in the image (distroless images need an exec probe); a named port is resolved against the container's ports and an unknown name is an error. `startupProbe` maps to `HealthStartupCmd=` and its own timing keys. `tcpSocket`, `grpc`, unsupported ports/headers/hosts and a `startupProbe` without a main probe are rejected with 422. See [configuration.md](configuration.md#health-checks-probes).
- **Port semantics**: `containerPort` is internal-only (namespace network); `hostPort` publishes to host via `PublishPort=`; a Service never binds a port, it maps selector + `targetPort` to DNS aliases and ingress backends. Ingress backends reach non-hostNetwork pods by their `<pod>.<ns>` DNS name on the namespace network; an explicit `hostPort` keeps its host publish.
- **CrashLoopBackOff**: detected from restart count + non-zero exit, shown in pod status
- **StartLimitBurst=5**: systemd gives up after 5 failures within 60s
- **Delete cascade**: deployment delete removes owned pods, stops units, removes quadlets
- **Namespace delete**: stops the namespace's workload units, then stops its `{namespace}-{pvc}-volume.service` units, removes the `.volume` Quadlets and keeps the Podman volumes for `q8s doctor` to report; also stops `q8s-{ns}-network.service`, removes the `q8s-{ns}.network` Quadlet and runs `podman network rm q8s-{ns}`. The `rm` has no `--force`, so a container still joined to the network makes it fail and leaves the network for `q8s doctor` to report
- **Deployment restore**: on restart, deployment recreated from container labels if missing
- **command and args**: `command` replaces the image ENTRYPOINT (`Entrypoint=`) and `args` replaces CMD (`Exec=`), as in Kubernetes
- **Pod labels**: keys under `io.kubernetes.`, `io.podman.`, `io.containers.`, `PODMAN_` or `q8s.io/`, and `app.kubernetes.io/managed-by`, are rejected with 422
- **ConfigMap and Secret size**: the values of `data` plus `binaryData`, or `data` plus `stringData`, are limited to 1048576 bytes on create, update and patch
- **creationTimestamp**: set on create for every stored kind and preserved on update; a ConfigMap, Secret, PVC or Service loaded from an older store without one gets the store file's modification time
- **ServiceAccount credentials**: see [ServiceAccounts in pods](rbac.md#serviceaccounts-in-pods)
