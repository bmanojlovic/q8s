package main

import (
	"fmt"
	"os"
)

const installUsage = `Usage: q8s install [flags]

Generates certificates, creates directories and installs the systemd units.

Flags:
  --name <name>        Cluster/context name for kubeconfig (default: hostname)
  --san-ip <ip>        Add extra IP to server certificate SAN
  --san-dns <name>     Add extra DNS name to server certificate SAN
  --server <url>       Server URL for kubeconfig (default: https://localhost:{port})
  --regenerate-certs   Force certificate regeneration
  -h, --help
`

const uninstallUsage = `Usage: q8s uninstall [--yes] [--purge]

Stops q8s and removes its systemd units, quadlet files and Podman objects.
Podman volumes are kept. Without --purge the data directory keeps store.json
and store.json.bak so 'q8s install && q8s serve' can restore the resources.

Flags:
  --yes     Do not ask for confirmation
  --purge   Remove the whole data directory as well
  -h, --help
`

const statusUsage = `Usage: q8s status

Shows whether the q8s socket and API server are active and the API port is
reachable. Exits 1 when q8s.socket is not active.
`

const doctorUsage = `Usage: q8s doctor [--json]

Reports what this host gives q8s workloads and what it does not.
Read-only: it never writes files or starts containers.

Flags:
  --json   Print the report as JSON
  -h, --help
`

const migrateNamesUsage = `Usage: q8s migrate-names [--dry-run] [--to-legacy]

Migrates the host artifacts of stored objects to the new generated names.

Flags:
  --dry-run    Print what the startup migration would stop, remove and start
  --to-legacy  Remove the new-format artifacts so the previous release can run
  -h, --help
`

const startUsage = `Usage: q8s start

Starts q8s.socket so q8s begins accepting connections.
`

const stopUsage = `Usage: q8s stop

Stops q8s.socket and the q8s API server.
`

const enableUsage = `Usage: q8s enable

Enables q8s.socket and starts it now, so q8s comes back on boot.
`

const disableUsage = `Usage: q8s disable

Stops q8s.socket and removes it from boot.
`

const serveUsage = `Usage: q8s serve

Runs the q8s API server in the foreground. It is normally started by
systemd through q8s.socket.
`

const kubeconfigUsage = `Usage: q8s kubeconfig [flags]

Without --user, prints the administrator kubeconfig created by q8s install.

Flags:
  --name NAME            Cluster and context name (default: the name from q8s install)
  --server URL           Server URL (default: the URL from q8s install)
  --user NAME            Issue a client certificate and kubeconfig for NAME
  --group GROUP          Add GROUP to the certificate; repeat for several groups
  --days N               Certificate lifetime in days (default 365, max 3650)
  --revoke NAME          Refuse future requests from certificate user NAME
  --revoke-serial HEX    Refuse future requests from certificate serial HEX
  --revoked              List revoked users and serials
`

const credentialUsage = `Usage: q8s credential

Prints the ExecCredential for the service account of the pod this command
runs in. q8s serve writes a kubeconfig that invokes it; running it outside a
q8s pod fails.

The socket is $Q8S_WORKLOAD_SOCKET, or /var/run/q8s/workload/sa.sock.
`

// command is one q8s subcommand. The table below is both the dispatcher and
// the source of the usage texts printUsage lists, so a command cannot be
// added without a usage entry.
type command struct {
	name    string
	summary string
	usage   string
	run     func()
}

var commands = []command{
	{name: "serve", summary: "Run the q8s API server (used by systemd)", usage: serveUsage, run: cmdServe},
	{name: "install", summary: "Generate certs, create dirs, install systemd units", usage: installUsage, run: cmdInstall},
	{name: "uninstall", summary: "Remove q8s installation", usage: uninstallUsage, run: cmdUninstall},
	{name: "status", summary: "Show q8s server status and connectivity", usage: statusUsage, run: cmdStatus},
	{name: "doctor", summary: "Report host capabilities and limits for workloads (read-only)", usage: doctorUsage, run: cmdDoctor},
	{name: "migrate-names", summary: "Migrate host artifacts to the new generated names, or roll back", usage: migrateNamesUsage, run: cmdMigrateNames},
	{name: "start", summary: "Start the q8s socket (begin accepting connections)", usage: startUsage, run: cmdStart},
	{name: "stop", summary: "Stop the q8s socket and API server", usage: stopUsage, run: cmdStop},
	{name: "enable", summary: "Enable and start q8s socket on boot", usage: enableUsage, run: cmdEnable},
	{name: "disable", summary: "Disable q8s socket (stop and remove from boot)", usage: disableUsage, run: cmdDisable},
	{name: "kubeconfig", summary: "Print a kubeconfig file for this q8s instance", usage: kubeconfigUsage, run: cmdKubeconfig},
	{name: "credential", summary: "Print the service account ExecCredential for this pod", usage: credentialUsage, run: cmdCredential},
}

func commandFor(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// usageFor returns the usage text of one subcommand.
func usageFor(name string) (string, bool) {
	c, ok := commandFor(name)
	if !ok {
		return "", false
	}
	return c.usage, true
}

// helpRequested reports whether -h or --help appears after the subcommand.
func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printUsage() {
	fmt.Println("Usage: q8s <command>")
	fmt.Println()
	fmt.Println("Commands:")
	for _, c := range commands {
		gap := ""
		if len(c.name) >= 11 {
			gap = "  "
		}
		fmt.Printf("  %-11s%s%s\n", c.name, gap, c.summary)
	}
}

// requireNoArguments refuses any argument of a subcommand that takes none.
func requireNoArguments(usage string, args []string) {
	if len(args) == 0 {
		return
	}
	for _, arg := range args {
		fmt.Fprintf(os.Stderr, "unknown argument %q\n", arg)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
