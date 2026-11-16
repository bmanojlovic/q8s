# RBAC and user identities

q8s authenticates every API request with mutual TLS and authorizes it against
Role, RoleBinding, ClusterRole and ClusterRoleBinding objects. The default
kubeconfig from `q8s install` is an administrator kubeconfig; additional
identities are issued with `q8s kubeconfig --user`.

## Identity

The verified client certificate carries the identity:

| Certificate field | Meaning |
|---|---|
| Subject CommonName | user name |
| Subject Organization, one entry per group | groups |
| `system:authenticated` | added to every authenticated request |

A certificate issued by `q8s install` has `CN=q8s-user` and `O=q8s-user`.
Members of `system:masters` or `q8s-user` are administrators: they bypass
authorization completely, may impersonate other users, may write RBAC objects
and may create SubjectAccessReviews. A `--group system:masters` or
`--group q8s-user` kubeconfig is therefore a full administrator credential;
`q8s kubeconfig` prints a warning when one is requested.

All other users are authorized per request. The verb, API group, resource,
subresource, namespace and name come from the request path and method, the
same way the Kubernetes authorizer builds them:

| Request | Verb |
|---|---|
| `GET` collection | `list` |
| `GET` object, or collection with `?watch=true` | `get` / `watch` |
| `POST` | `create` |
| `PUT` | `update` |
| `PATCH` | `patch` |
| `DELETE` object / collection | `delete` / `deletecollection` |
| pods `exec`, `attach`, `portforward` | `create` |
| pods `log` | `get` |

Rule matching follows Kubernetes: `*` matches every verb, API group and
resource; `resourceNames` is an exact list of names and never grants a request
without a name, so it cannot grant a collection request but can grant `exec`,
`attach` or `portforward` (verb `create`) on the named pod it lists; `pods/*`
grants every pod subresource, and a rule naming `pods` does not grant
`pods/log`. A `POST` to a named object URL is answered with 405
MethodNotAllowed after authentication and authorization, so an unauthenticated
request still gets 401 and an unauthorized one 403; only the collection URL
creates an object, and the named subresources that take POST (`pods/exec`,
`pods/attach`, `pods/portforward`, `pods/eviction`, bindings) are the
exception. A RoleBinding
may point at a Role in its namespace or at a ClusterRole, and the ClusterRole
rules then apply only in that namespace. ClusterRoleBindings apply in every
namespace. Aggregation through `aggregationRule` is evaluated at request time.
The built-in roles aggregate as in Kubernetes: `view` is part of `edit` and
`edit` is part of `admin`, so a ClusterRole labelled
`rbac.authorization.k8s.io/aggregate-to-view: "true"` reaches all three.
`nonResourceURLs` rules are honored only through ClusterRoleBindings.

The following requests are open to every authenticated user:

- discovery: `GET /api`, `/api/v1`, `/apis`, `/apis/<group>`,
  `/apis/<group>/<version>`, `/version`, `/healthz`, `/openapi/v2` and
  `/openapi/v3/...`; nothing else under `/api` or `/apis` counts as discovery
- `SelfSubjectAccessReview`, `SelfSubjectRulesReview` and
  `SelfSubjectReview` creation

Writes to `rbac.authorization.k8s.io` objects are administrator-only.
Escalation checks are not implemented, so a bound rule that appears to grant
them is not enough. `SubjectAccessReview` creation is administrator-only as
well, because it reviews another user's access.

## Host access

Host access is administrator-only, independent of the RBAC verbs a user
holds. A non-administrator who creates, updates or patches a Pod, Deployment,
Job, CronJob or PersistentVolumeClaim that uses any of these fields gets 403
Forbidden, even when the change itself leaves the host-access field untouched.
Scaling is not a host-access change: a non-administrator with `patch` or
`update` on `deployments/scale` may scale a Deployment whose pod template has
host access, including to zero, because only `spec.replicas` changes. Deletes
remain governed by RBAC.

- any container or initContainer with
  `securityContext.privileged: true`
- `spec.hostPID: true`
- `spec.hostIPC: true`
- `spec.hostNetwork: true`
- `spec.hostUsers: true`
- a PersistentVolumeClaim whose resolved storage class is `hostpath` (an
  empty `storageClassName` resolves to `standard`, so it is not host access)
  or, on such a claim, the `q8s.io/host-path` annotation
- the `q8s.io/attach-all-networks: "true"` annotation on a Pod or on a pod
  template

These fields either disable the rootful user namespace, join a host namespace
or join every namespace network, so they are equivalent to root on the host: a
q8s administrator is trusted like root on the machine (or the owner of the
account, rootless).
A pod of an ordinary user may still mount a hostpath PersistentVolumeClaim
that an administrator created in its namespace; creating the claim is the
administrator's decision to hand that directory to the namespace. The GPU device
(`nvidia.com/gpu`) and the read-only ServiceAccount workload mounts are not
host access and stay available to ordinary users. The same check applies to
server-side apply and to dry-run requests; it runs on the resulting object, so
a patch that only changes an image still fails while the pod template keeps
host access. Existing objects are not changed at startup or during reconcile.

An API path with an empty, `.` or `..` segment, or an encoded `/` or `.`, is
refused with 400 before authorization. A write whose object names a different
namespace or name than the URL is refused with 400, as in Kubernetes, so a
binding in one namespace never lets a user create objects in another.

Generated Podman and systemd names are `<namespace>-<name>`. To stop that join
from crossing namespaces, creating an object whose kind generates such a name
is refused with 409 Conflict when the flattened name already belongs to an
object of another namespace. The guarded kinds are Pod, Deployment, Job,
CronJob, PersistentVolumeClaim, Service, Ingress, Middleware and
IngressRouteTCP; pods created for a Deployment go through the same check. The
`pods/log`, `pods/exec` and `pods/attach` subresources address only a pod
stored in the URL namespace, or a Job/CronJob run resolved there, and
`?container=` must name one of that pod's containers. A user permitted in one
namespace therefore cannot reach a pod in another namespace whose flattened
name happens to match.

A denied request answers 403 with the Kubernetes message, for example:

```text
pods is forbidden: User "alice" cannot get resource "pods" in API group "" in the namespace "default"
```

## Served RBAC objects

`rbac.authorization.k8s.io/v1` serves `Role`, `RoleBinding`, `ClusterRole` and
`ClusterRoleBinding` with get, list, watch, create, update, patch, delete and
deletecollection. `kubectl get roles,rolebindings -A` works, and deleting a
namespace deletes its Roles and RoleBindings.

Validation at the API:

- every rule needs at least one verb, and either resources with API groups or
  `nonResourceURLs`; `nonResourceURLs` are allowed only in a ClusterRole
- `roleRef.apiGroup` must be `rbac.authorization.k8s.io`; `roleRef.kind` is
  `Role` or `ClusterRole`, and a ClusterRoleBinding accepts only `ClusterRole`
- `roleRef` cannot be changed by an update or patch
- subjects must be `User`, `Group` or `ServiceAccount`

The built-in ClusterRoles `cluster-admin`, `admin`, `edit` and `view` are
synthetic: they are served by get and list, carry the
`kubernetes.io/bootstrapping: rbac-defaults` label and are read-only. Their
rules follow the Kubernetes defaults for the resources q8s serves. `view` can
read workloads, logs, services, ConfigMaps, nodes and metrics but not Secrets;
`edit` adds writes to workloads (including `deployments/scale`) plus full read of Secrets; `admin` adds read
of Roles and RoleBindings in a namespace; `cluster-admin` is the wildcard.

## User kubeconfigs

Run these on the host that holds the q8s CA (`{dataDir}/certs`):

```sh
(umask 077; q8s kubeconfig --user alice --days 30 > alice.kubeconfig)
(umask 077; q8s kubeconfig --user alice --group dev --group ops > alice.kubeconfig)
(umask 077; q8s kubeconfig --user alice --name demo --server https://192.0.2.10:6443 > alice.kubeconfig)
```

- `--user NAME` issues a client certificate with `CN=NAME`, one Organization
  entry per `--group`, and a kubeconfig whose user and context are
  `NAME@<cluster>`.
- `--days N` sets the lifetime, 365 days by default, at most 3650.
- `--name` overrides the cluster and context name, `--server` the server URL.
  Both also apply to the administrator kubeconfig printed without `--user`.
  `--server` must be an `https://` URL.
- Without `--user`, `q8s kubeconfig` prints the administrator kubeconfig that
  `q8s install` created.
- The certificate serial is printed on stderr; use it with `--revoke-serial`.
- Certificates are not rotated. Revoke an identity instead of waiting for it
  to expire.

## Revocation

```sh
q8s kubeconfig --revoke alice
q8s kubeconfig --revoke-serial 1a2b3c4d5e6f
q8s kubeconfig --revoked
```

`--revoke` adds a user name, `--revoke-serial` a lowercase hexadecimal serial,
and `--revoked` prints the list. A user revocation matches the certificate
CommonName, so it covers every certificate issued for that user; a serial
revocation covers one certificate, and it is permanent: it also rejects
certificates issued later, so `q8s kubeconfig --user` refuses a revoked name.
Issue the new kubeconfig under another user name. The file is `{dataDir}/revoked.json`
(rootful `/etc/q8s/revoked.json`, rootless
`~/.local/share/q8s/revoked.json`), written with mode 0600 through a temporary
file and rename. The server checks it on every request and re-reads it when
its modification time or size changes, so a revocation applies immediately
without a restart. A missing file means nothing is revoked. An unparseable
file keeps the last good list and logs a warning.

A revoked certificate gets 401 `client certificate revoked`. Revoking
`q8s-user` by name is refused because it is the administrator identity
created by `q8s install`. `--revoke`, `--revoke-serial` and `--revoked` are
mutually exclusive and cannot be combined with `--user`, `--group` or
`--days`.

## Impersonation

Administrators may set `Impersonate-User` and one or more `Impersonate-Group`
headers, which `kubectl --as` and `--as-group` send:

```sh
kubectl auth can-i get pods -n demo --as=alice
kubectl auth can-i --list -n demo --as=alice
kubectl --as=alice --as-group=dev auth whoami
```

For anyone else an `Impersonate-*` header answers 403. `Impersonate-Group`
requires `Impersonate-User`, and `system:authenticated` is added to the
impersonated groups. `Impersonate-Uid` is accepted and ignored because q8s
identities carry no UID. The impersonated identity is what the access reviews,
`kubectl auth whoami` and authorization see.

## Access reviews

| Resource | Command | Who |
|---|---|---|
| `selfsubjectaccessreviews` | `kubectl auth can-i VERB RESOURCE` | every authenticated user |
| `selfsubjectrulesreviews` | `kubectl auth can-i --list` | every authenticated user |
| `selfsubjectreviews` | `kubectl auth whoami` | every authenticated user |
| `subjectaccessreviews` | raw `SubjectAccessReview` object | administrators |

The reviews are create-only and never stored. `SelfSubjectRulesReview` lists
the rules from the caller's RoleBindings in the requested namespace plus every
ClusterRoleBinding, resolved through the same roleRef and aggregation lookup
the authorizer uses, and adds the self-review rules every user holds. An
administrator gets a wildcard rule. The list describes bound rules; the
administrator-only restrictions on RBAC writes and SubjectAccessReviews still
apply to the requests themselves. `SubjectAccessReview` evaluates the user and
groups in its spec, and is the way to check access for somebody else:

```yaml
apiVersion: authorization.k8s.io/v1
kind: SubjectAccessReview
spec:
  user: alice
  groups: ["system:authenticated"]
  resourceAttributes:
    namespace: demo
    verb: get
    group: ""
    resource: pods
```

## Worked example

Create a user kubeconfig, grant `view` in one namespace, check it, then revoke
it. The first command runs on the q8s host.

```sh
(umask 077; q8s kubeconfig --user alice --days 30 > alice.kubeconfig)

kubectl create namespace demo
kubectl -n demo create rolebinding alice-view --clusterrole=view --user=alice

kubectl --kubeconfig alice.kubeconfig auth can-i get pods -n demo
# yes
kubectl --kubeconfig alice.kubeconfig auth can-i create pods -n demo
# no
kubectl --kubeconfig alice.kubeconfig auth can-i get pods -n default
# no
kubectl --kubeconfig alice.kubeconfig auth can-i --list -n demo
kubectl --kubeconfig alice.kubeconfig auth whoami
kubectl --kubeconfig alice.kubeconfig get pods -n demo
```

`view` is limited to the `demo` namespace, so the same user has no access in
`default`. Revocation takes effect on the next request:

```sh
q8s kubeconfig --revoke alice
q8s kubeconfig --revoked
kubectl --kubeconfig alice.kubeconfig get pods -n demo
# Error from server (Unauthorized): client certificate revoked
```

## ServiceAccounts in pods

`ServiceAccount` objects are served (`sa` is the short name) and are stored
without token Secrets. A pod that sets `serviceAccountName` gets credentials on
demand:

- q8s mounts two host directories read-only:
  `/var/run/q8s/workload` (kubeconfig and credential socket) and
  `/var/run/q8s/bin` (the q8s binary used as the exec credential plugin).
- `KUBECONFIG` points at `/var/run/q8s/workload/ns/<namespace>/kubeconfig`,
  whose context namespace is the pod's namespace. A container that sets
  `KUBECONFIG` itself keeps its value; the volume is still mounted.
- That kubeconfig contains no secret. Its user runs the exec credential plugin
  `/var/run/q8s/bin/q8s credential`, which asks
  `/var/run/q8s/workload/sa.sock` for a certificate.
- The server reads the peer pid from the socket, maps its cgroup to a container
  id, reads the container labels and finds the stored pod. The pod must name a
  ServiceAccount, must not set `automountServiceAccountToken: false`, and the
  ServiceAccount must exist. Only then is a certificate issued.

q8s does not create a `default` ServiceAccount in a namespace. Pod creation is
not blocked when the named ServiceAccount is missing; the pod starts and its
credential requests are refused until the ServiceAccount exists. Create the
ServiceAccount first.

The certificate is valid for one hour and identifies the pod as
`system:serviceaccount:<namespace>:<name>` with the groups
`system:serviceaccounts` and `system:serviceaccounts:<namespace>`
(`system:authenticated` is added by the certificate handling like for any
other user). Bind a Role or ClusterRole to that ServiceAccount subject, or to
the namespace group:

```yaml
subjects:
  - kind: ServiceAccount
    name: runner
    namespace: demo
  # or, for every ServiceAccount in the namespace:
  # - kind: Group
  #   name: system:serviceaccounts:demo
```

Nothing secret is written to disk, there is no token controller and no
rotation: the exec plugin asks again when its certificate expires. The API
stays mTLS-only. `q8s serve` logs the socket path and SELinux label at startup
and one line per credential decision (caller, issued or refused and why); the
certificate itself is never logged.

The host side:

| Path | Contents |
|---|---|
| `{dataDir}/workload-bin/q8s` | copy of the q8s binary, rootful `/etc/q8s/workload-bin/q8s`, rootless `~/.local/share/q8s/workload-bin/q8s` |
| `/run/q8s/workload` | rootful workload directory, rootless `$XDG_RUNTIME_DIR/q8s/workload` |
| `/run/q8s/workload/kubeconfig` | host-wide kubeconfig |
| `/run/q8s/workload/ns/<namespace>/kubeconfig` | per-namespace kubeconfig used by pods |
| `/run/q8s/workload/sa.sock` | credential socket, mode 0666 |

The binary copy is replaced only when it differs, and every file is written
atomically. Both mounts are directories, so a q8s upgrade that replaces the
binary, kubeconfig or socket is visible to running pods without a restart.

The paths `/var/run/q8s/workload` and `/var/run/q8s/bin` are reserved: a pod
whose container mounts a volume there is rejected with 422, even when no
ServiceAccount is used.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: runner
  namespace: demo
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: pod-reader
  namespace: demo
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: runner-pod-reader
  namespace: demo
subjects:
  - kind: ServiceAccount
    name: runner
    namespace: demo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: pod-reader
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: pod-report
  namespace: demo
spec:
  schedule: "*/5 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: runner
          restartPolicy: Never
          containers:
            - name: report
              image: bitnami/kubectl:1.36.2
              command: ["sh", "-c", "kubectl get pods -n demo"]
```

Job and CronJob runs are identified by their `io.kubernetes.job.name` or
`io.kubernetes.cronjob.name` container label, so the CronJob above gets a
credential for `runner` on every run. The same ServiceAccount, Role and
RoleBinding work with a Pod or a Deployment whose pod template sets
`serviceAccountName: runner`.

Each certificate is bound to the workload that asked for it: it carries a URI
SAN `urn:q8s:workload:<pod|job|cronjob>:<namespace>/<name>/<uid>`, and every
request made with it is checked against the store. The workload must still
exist with that UID and still use that ServiceAccount, and the ServiceAccount
must exist; otherwise the request is 401. Deleting the pod, Job, CronJob or
ServiceAccount, or changing `serviceAccountName`, therefore revokes the
certificate at once, without waiting for its one-hour expiry.

On an SELinux host the listening socket is created with type `spc_t`, so a
confined container (`container_t`) can connect without a policy change. If the
label cannot be set, q8s logs a warning and the pod gets a permission error
when it asks for a credential. Set `Q8S_WORKLOAD_SOCKET` to point
`q8s credential` at another socket path.
