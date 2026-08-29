# Backlog — issues found in real use

## RESOLVED (as of v0.3.5, 2026-08-29)

All four issues below (#1-#4) are fixed. Full mozak-brain deploy via
Terraform now works end-to-end on both instances (local rootless,
VM rootful) — verified live: PVC binds immediately, namespace-scoped
volume naming, Secret/Deployment/Pod/ConfigMap/Job/CronJob/Ingress
PATCH all handle both merge-patch and RFC 6902 JSON Patch, install is
idempotent with reliable cert reload.

## 1. `standard` StorageClass provisioner isn't binding new PVCs — FIXED (`7f4bc39`)

Deploying mozak-brain via Terraform (2026-08-29, local rootless q8s
instance), a PVC never bound, `StorageClassName` empty. Worked through
two hypotheses, both real but not the root cause:

- `standard`'s StorageClass carries
  `storageclass.kubernetes.io/is-default-class: "true"` (confirmed via
  `kubectl get storageclass -o yaml`), but a PVC created with no
  `storageClassName` never gets it auto-populated the way real
  Kubernetes' `DefaultStorageClass` admission plugin would — worth
  fixing on its own (an inert annotation that looks like working
  behavior is worse than no annotation), but not the actual blocker
  here, since explicitly setting `storageClassName: standard` didn't
  fix it either.
- Suspected a stale Podman volume name collision (see #2) — ruled
  out: a brand-new, never-before-used PVC name (`storageClassName:
  standard` set explicitly) *still* didn't bind, and `podman volume
  ls` confirms the underlying volume was never even created.

So the actual issue: the `standard` provisioner (`q8s.io/podman-
volume`) isn't provisioning/binding *any* new PVC on this instance
right now, regardless of naming or explicit storageClassName. No
error surfaces anywhere client-visible (`kubectl describe pvc` shows
nothing, no events) — whatever's failing is silent, server-side.
Worth checking whether the provisioner controller is actually running
as a distinct process/goroutine, or whether it's erroring and
swallowing the error instead of surfacing it as a PVC event the way
real Kubernetes would.

**Confirmed systemic, not a local/rootless quirk**: reproduced the
identical failure signature the same day on the *other* q8s instance
— rootful, VM-hosted, freshly installed at v0.3.1 (vs. the rootless
instance's already-running install). Same symptom exactly:
`storageClassName: standard` set explicitly, PVC never binds, no
events, underlying Podman volume never created. Two independent
instances, two install modes (rootless/rootful), same version, same
result — this rules out anything host-specific and points at the
provisioner logic itself.

## 4. q8s v0.3.1 install / cert-regeneration issues — FIXED (`f0f93b1`)

While debugging #1 on the VM, hit three more real bugs in the
install/kubeconfig path itself, all confirmed live 2026-08-29:

- `q8s install` (regenerating certs for new SANs) reports "Restarted
  q8s-api.service to load new certificates" but the restart doesn't
  reliably take effect — the server kept serving its OLD certificate
  (verified by comparing the live TLS handshake's cert fingerprint
  against the freshly-written `/etc/q8s/certs/server.crt` on disk)
  until an explicit external `systemctl restart q8s-api.service`.
- `q8s kubeconfig --name X` produces a context/cluster both correctly
  named `X`, but the **user** entry is named `X-user` — not documented
  anywhere obvious, easy to get wrong when scripting around it (cost
  real debugging time: a cleanup script targeting `users.X` silently
  missed the actual `X-user` entry).
- Re-running `q8s install`/`q8s kubeconfig` regenerates the CA/client
  cert every time (even with an unchanged SAN list) rather than
  detecting "no change needed" and skipping — combined with the
  above, a caller that merges repeated `q8s kubeconfig` fetches into
  one local file (the normal `kubectl config view --flatten` pattern)
  accumulates stale, unusable cluster/user entries under the *same*
  context name unless it explicitly deletes the prior entries first.
  Worth either making install idempotent when nothing changed, or
  documenting clearly that every kubeconfig fetch is a fresh identity
  the caller must replace, not merge.

The annotation-not-honored point above is still real and worth its own
fix separately, once the binding itself works at all.

## 2. PVC → Podman volume name has no namespace scoping — FIXED (`bb66806`)

`persistentvolumeclaims` docs (`docs/resources.md`) already say
"Named Podman volume" — confirmed: the PVC's bare `metadata.name`
becomes the Podman volume name directly, with no namespace or
cluster-scoping prefix. Two PVCs named the same thing in *different*
namespaces (or from entirely separate tools/deployments on the same
host) silently collide on one underlying volume.

This caused a real near-miss tonight: a Terraform-managed PVC named
`mozak-data` in the `default` namespace mapped onto the exact same
Podman volume a long-running standalone (non-q8s) Podman container
was actively using as its real, live data volume — 46+ hours of real
production use, not a test artifact. It was one `podman volume rm`
away from being deleted by mistake, having been misdiagnosed as an
orphaned leftover from an unrelated, already-torn-down test (a
plausible mistake specifically *because* nothing in the volume name
or metadata indicates which namespace/PVC currently claims it).

Real Kubernetes CSI provisioners generate globally-unique PV/volume
names (typically prefixed/suffixed with a generated ID) specifically
to avoid this. `standard`/`standard-shared` should do the same —
e.g. `{namespace}-{pvc-name}` or a generated suffix — rather than
reusing the PVC's own name verbatim. Bonus: this would also make
`podman volume ls` self-documenting about which namespace/claim owns
a given volume, instead of requiring a live cross-reference against
still-running k8s objects to tell "orphaned" from "in use."

**Fixed as `{ns}-{name}`** (e.g. `default-mozak-data`). Confirmed this
also means an existing standalone (non-k8s) volume of the exact same
bare name no longer collides — verified live on both instances.

## 3. Secret update (PATCH) is broken — FIXED (`8017c35`, generalized `2b5a6f0`)

`docs/resources.md` lists `secrets` as supporting `patch`. Confirmed
broken live 2026-08-29: `kubernetes_secret_v1` create works fine, but
updating an existing Secret's `data` (via `terraform apply` after a
`data` change) fails server-side:

```
Failed to update secret: json: cannot unmarshal array into Go value of type map[string]interface {}
```

Root cause: Terraform's kubernetes provider sends an RFC 6902 JSON
Patch (`application/json-patch+json`, a top-level array of ops) for
this kind of change — since a merge-patch can't express deleting a
map key — but every PATCH handler in the server only ever tried to
`json.Unmarshal` the body straight into `map[string]interface{}`,
which chokes on a top-level array.

Fixed for Secret first (`8017c35`, plus stale per-key file cleanup on
update), then generalized to Pod/Service/PVC/ConfigMap/Deployment/
Job/CronJob/Ingress (`2b5a6f0`) after the exact same crash reproduced
on `kubernetes_deployment_v1` mid-deploy (`PATCH {"spec":...}` sent as
a JSON Patch array once a container image diff was involved). All
PATCH handlers now branch on `Content-Type: application/json-patch
+json` and apply an RFC 6902 op-list (`internal/server/jsonpatch.go`)
instead of assuming merge-patch unconditionally.

## New issues found completing the mozak-brain deploy (2026-08-29, v0.3.2→v0.3.5)

All fixed same day, released as v0.3.2 through v0.3.5:

- **`HealthStartPeriod=<seconds>` had no unit suffix** (`8d72701`) —
  podman rejected it outright (`time: missing unit in duration "10"`,
  exit 125) whenever a pod's LivenessProbe set `initialDelaySeconds`.
- **`deploymentReplicas()` floored an explicit `replicas: 0` to `1`**
  (`fdf3427`) — broke the old/new replica diff used to decide whether
  to create/remove pod instances; a `PATCH {"spec":{"replicas":0}}`
  came back reporting `replicas: 1`, making scale-to-zero (and a
  subsequent scale-up) silently no-op.
- **`Env[i].ValueFrom.{configMapKeyRef,secretKeyRef}` was never
  resolved** (`bbdeb6d`) — only bulk `envFrom` was, so any pod using
  the more common per-var form got a blank `Environment=` line in the
  generated quadlet. The app then exits cleanly on missing required
  config instead of crash-looping visibly, so this manifested as a
  pod stuck `Succeeded` with 0 restarts — much harder to diagnose than
  a crash loop would have been.
- **`Restart=on-failure` was hardcoded for `RestartPolicy: Always`
  pods** (`bbdeb6d`) — a container that exits 0 (e.g. because of the
  bug above) never restarts under `on-failure`, so the pod just sits
  `Succeeded` forever even though the Deployment wants it running.
- **Secret-derived env values were inlined into the 0644 quadlet
  `.container` file** (`1a54b52`) — world-readable-ish, unlike the
  0600 per-key files `writeSecretFiles` already uses for volume-mounted
  secrets. Now split into a 0600 `EnvironmentFile=` next to those.
- **A missing non-optional ConfigMap/Secret/key ref silently rendered
  as an empty env value** (`1a54b52`) instead of failing — real
  Kubernetes refuses to start such a pod (`CreateContainerConfigError`)
  rather than running it with a blank value. Now returns an error and
  skips quadlet generation instead (optional refs still degrade
  gracefully, as before).

## Not a q8s bug: podman quadlet `Memory=` key version gap

The VM's podman (5.4.2, SUSE-packaged) rejects the `Memory=` quadlet
key outright — `unsupported key 'Memory' in group 'Container'` from
`podman-system-generator`, and the unit never gets generated at all
(no journal entries, pod just never starts). Local's podman (6.0.2)
accepts it fine. `internal/quadlet/generator.go`'s existing
`cgroupMemoryAvailable()` guard only checks cgroup *delegation*
(rootless vs rootful), not whether the *quadlet syntax itself* is
supported by the host's podman version — those are separate concerns
that happened to look like the same thing.

Not fixed in q8s itself (would need a real capability probe — e.g. a
dry-run generation attempt — rather than a guessed version cutoff,
which risks being wrong for some other patch release). Worked around
on the mozak/Terraform side instead: `mozak-brain` module's
`memory_limit` variable can be set to `""` to omit the `resources.
limits` block entirely. If this comes up again on another host, worth
building a real probe here instead of pushing the workaround to every
caller.
