# q8s doctor

`q8s doctor` is a read-only host capability report. It answers one question for
the operator: what does this machine give my workloads, and what does it not?

The command never writes a file, starts a container, runs `podman run`, or
changes a setting. The only external commands it runs are `podman version`,
`podman info`, and (rootless only) `loginctl` through `install.LingerEnabled`.
Everything else is a read of an existing file or a `statfs` call.

```sh
q8s doctor          # human report
q8s doctor --json   # the same Report as indented JSON
q8s doctor -h       # usage
```

The process exit code is `0` when every check is ok or informational, `1` when
any check warns, and `2` when any check fails. An unknown flag prints usage on
stderr and exits 2.

## Reading the output

Every check prints a status, a stable id, and the facts that were found:

- `[ok]` means the capability is present and workloads keep the benefit.
- `[info]` means the check is informational, not a problem.
- `[warn]` means workloads keep running but lose a protection or a feature.
- `[fail]` means q8s cannot work correctly until this is fixed.

`gain:` lines appear only for ok checks. `lose:` and `fix:` lines appear only
for warn and fail checks. The fix is an exact command (or commands) to copy.

## Example

```text
$ q8s doctor
q8s doctor: rootful, podman 5.8.2, kernel 6.12.0-160100.8-default

[ok]   podman        podman 5.8.2
       gain: q8s can run workloads
[info] kernel        6.12.0-160100.8-default
[warn] userns        no 'containers' entry in /etc/subuid, /etc/subgid
       lose: container uids are host uids: root in a container is real host root, and two containers using the same uid share an identity
       fix:  q8s serve adds it on its next start (or by hand: echo containers:2147483647:2147483648 >> /etc/subuid && echo containers:2147483647:2147483648 >> /etc/subgid)
[ok]   idmap         /var/lib/containers/storage=xfs, /etc/q8s=xfs, /run/q8s=tmpfs, /etc/q8s/traefik=tmpfs
       gain: volumes keep their host owners inside a user namespace, no chown needed
[ok]   cgroups       cpu, memory and pids are delegated
       gain: resources.limits are enforced
[ok]   network       published ports use kernel NAT; client IPs are preserved
       gain: published ports use kernel NAT; client IPs are preserved
[ok]   certificates  SANs: DNS:localhost, DNS:q8s, IP:127.0.0.1; expires 2027-08-01T12:00:00Z
       gain: the q8s API certificate is trusted until 2027-08-01
[ok]   traefik-dir   /etc/q8s/traefik is writable
       gain: Ingress changes can be written immediately

summary
  isolation     host uids (weak)
  limits        enforced
  networking    published ports use kernel NAT; client IPs are preserved
  persistence   systemd system units, start at boot
  certificates  valid until 2027-08-01
```

## The checks

### `podman`

Runs `podman version --format {{.Client.Version}}`. Podman 5 or newer is
required because q8s writes Quadlet `[Pod]` units and keys that older podman
does not understand. Missing or too old is a fail: nothing can run.

### `kernel`

Reports the uname release, for example `6.12.0-160100.8-default`. The major
and minor numbers are parsed for the idmap check, so distribution suffixes are
tolerated. This check is always informational.

### `userns`

Decides whether pods can get their own user namespace (`UserNS=auto`), which
needs a subordinate uid and gid range.

Rootful: looks for a `containers` entry in both `/etc/subuid` and
`/etc/subgid` with at least 65536 ids. With it, root inside a container is an
unprivileged uid on the host and each pod gets a separate range. Without it,
container uids are host uids, so root in a container is real host root and two
containers sharing a uid share an identity. Rootful `q8s serve` adds the
`containers` range itself on its next start, so the fix says that and also
shows the equivalent manual command. Too small a range gets a fix that replaces
those lines (`containers` is not a login user, so `usermod` cannot change it).

Rootless: looks for the current login name or numeric uid in both files. It is
a fail when missing, because rootless podman cannot run images that need more
than one uid. The fix uses `usermod --add-subuids` and `--add-subgids`.

### `idmap`

Only meaningful for rootful pods that use a user namespace. Rootless reports
`info`, because rootless containers already live in a user namespace.

Rootful checks the filesystem of podman's storage root (`podman info --format
{{.Store.GraphRoot}}`, falling back to `/var/lib/containers/storage`), the q8s
data directory, the q8s config directory (its parent when it does not exist
yet), and the Traefik directory when it already exists.

Kernel support:

| filesystem | idmap support |
| --- | --- |
| ext4, xfs, btrfs | kernel 5.12 or newer |
| tmpfs | kernel 6.3 or newer |
| overlay, zfs, nfs, other | not supported |

When every path is supported, volumes keep their host owners inside a user
namespace and no chown is needed. Otherwise volumes on the unsupported paths
cannot be idmapped and would be unreadable under a user namespace.

### `linger`

Rootless only; rootful does not emit this check because system units already
start at boot and survive logout.

`loginctl` decides whether the user manager keeps running after the last
session ends. Disabled linger is a warn: workloads stop at logout and do not
start at boot. The fix is `sudo loginctl enable-linger <user>`. If `loginctl`
cannot be read, the check is informational and says so.

### `cgroups`

Rootless reads
`/sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/cgroup.controllers`.
Rootful reads `/sys/fs/cgroup/cgroup.controllers`.

When `cpu`, `memory`, and `pids` are available, `resources.limits` are
enforced. A missing controller is a warn: the limit is written to the unit but
not enforced. The rootless fix is a systemd drop-in at
`/etc/systemd/system/user@.service.d/delegate.conf`:

```ini
[Service]
Delegate=cpu cpuset io memory pids
```

followed by `systemctl daemon-reload`. If the controllers file cannot be read,
the check warns that cgroup v2 was not detected.

### `network`

Rootless asks podman for `{{.Host.RootlessNetworkCmd}}` (for example `pasta` or
`slirp4netns`) and reports it as information. Published ports (`hostPort`) go
through that command in user space, which adds latency. Ingress through the
socket-activated Traefik pod still uses the kernel path.

Rootful is ok: published ports use kernel NAT and client IPs are preserved.

### `certificates`

Parses `<data dir>/certs/server.crt` and lists the certificate SANs and the
NotAfter time as RFC3339. Missing means q8s is not installed. Expired is a
fail. Expiring within 30 days is a warn. Both point at
`q8s install --regenerate-certs`.

### `traefik-dir`

The Traefik dynamic configuration directory is `<data dir>/traefik`. It is
created on the first Ingress, so a missing directory is only informational
(`no Ingress written yet`). If it exists but is not writable, Ingress changes
cannot be written and the check fails.

## Summary

The five summary lines are always present and are derived from the checks:

- `isolation`: rootful: own uid range per pod, or host uids (weak); rootless:
  the user's subordinate range, shared by all pods (container root is not host root).
- `limits`: enforced, or the list of controllers that are not enforced.
- `networking`: kernel NAT, or the rootless network command with its latency.
- `persistence`: linger state in rootless mode; system units in rootful mode.
- `certificates`: the NotAfter date, `expired`, or `missing`.

## JSON output

`q8s doctor --json` prints the `Report` with `mode`, `podman`, `kernel`,
`checks`, and `summary`. Each check has `id`, `title`, `status`, `detail`, and
optional `gain`, `loss`, and `fix` fields. The ids are stable and safe to match
in scripts:

`podman`, `kernel`, `userns`, `idmap`, `linger` (rootless only), `cgroups`,
`network`, `certificates`, `traefik-dir`.
