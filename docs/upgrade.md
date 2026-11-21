# Upgrading to the new generated names

q8s used to join a namespace and an object name with `-` for every host-side
name. That is ambiguous: `team/prod-db` and `team-prod/db` both became
`team-prod-db`. The current release joins them with `_`, which cannot occur in
a Kubernetes namespace or object name.

Hosts upgraded from a release that wrote the old names need a one-time
migration. q8s runs it automatically on the first `q8s serve` after the
upgrade, before any API request can be served.

## What the first start does

For every stored Pod, Deployment replica, Job and CronJob that still has old
artifacts, startup:

1. stops the old service, socket and timer units, and disables the old timer
   and socket units;
2. force-removes the old Podman container or pod;
3. removes the old Quadlet, socket, timer and env files;
4. runs one systemd daemon-reload.

The normal startup sequence then writes the new files and starts the new
units. Every workload restarts once during this upgrade. A completed Job is
not started again: it gets the new unit file for the record but stays
finished. A suspended CronJob keeps its timer disabled.

Traefik files are regenerated under the new names and the old files are
removed. Traefik reloads them itself, so no workload restarts for that.

When the whole startup sequence has finished, q8s writes a marker
`names-v2` next to `store.json`. While the marker is absent, every start
re-runs the migration and an ensure-started pass, so a crash during the
upgrade converges on the next start instead of leaving workloads down.

## PersistentVolumeClaims

PVC data is never copied, renamed or deleted. A claim keeps the Podman volume
named in its `spec.volumeName`: a claim created before the upgrade keeps its
old `<namespace>-<claim>` volume and `.volume` file forever. New claims use
the new `<namespace>_<claim>` name. The old release and the new release both
read the claim's `spec.volumeName`, so the data stays where it is.

Because Podman volumes cannot be renamed, rolling back a host that has
created new-format PVCs would make the previous release bind each claim to a
new, empty old-name volume. `q8s migrate-names --to-legacy` refuses in that
case and lists the affected claims. Copy their data to an old-format volume
first if a rollback is required.

## Preview the migration

The first start is automatic, but an operator can inspect the planned work
without changing anything:

```sh
q8s migrate-names --dry-run
```

The output lists the objects, units, containers, pods and files the next
startup would stop or remove, and the new units it would start. The command
changes no files and calls no systemd or Podman operation that mutates state.

Running `q8s migrate-names` without flags performs the same migration, writes
the new files and starts the workloads immediately. Stop `q8s serve` first:
the command refuses to change names while `q8s.socket` or `q8s-api.service`
is active.

## Roll back to the previous release

Prepare the host before installing the previous binary:

```sh
q8s stop
q8s migrate-names --to-legacy --dry-run   # preview
q8s migrate-names --to-legacy
```

`--to-legacy` stops the new-format units, removes the new containers, pods and
files, and removes the `names-v2` marker. It does not delete Podman volumes or
the `.volume` files of legacy claims. Afterwards it prints the next step:
install the previous q8s binary and start it. That release writes the old
files and starts the old units from the same store.

The previous release starts every workload whose quadlet file is missing,
so it runs finished Jobs and finished pods with `restartPolicy: Never` or
`OnFailure` once more. `--to-legacy` lists them; delete them before the
rollback if a second run matters.

Do not start the previous release before running `--to-legacy`: the old
release does not know the new names and can leave new-format containers and
files behind.

## Check for leftovers

`q8s doctor` reports leftovers. Its `legacy-names` check lists old-format
Quadlet, socket and timer files and old q8s-labeled containers of stored
objects that still exist, together with the exact `systemctl stop`,
`podman rm -f` and file removal commands. The `env-files` check lists an old
Job or CronJob env file that no stored workload uses. Doctor is read-only and
never removes them.
