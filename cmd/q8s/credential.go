package main

import (
	"fmt"
	"io"
	"os"

	"q8s/internal/server"
)

// defaultWorkloadSocket is where the kubeconfig q8s serve generates points
// its exec credential plugin. The server listens on this path inside pods.
const defaultWorkloadSocket = "/var/run/q8s/workload/sa.sock"

// cmdCredential prints the ExecCredential for the calling pod's service
// account. It is the exec credential plugin command in the workload
// kubeconfig, and can also be run by hand inside the pod.
func cmdCredential() {
	if err := runCredential(os.Args[2:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runCredential is cmdCredential without the process exit, so tests can
// exercise the command.
func runCredential(args []string, stdout, stderr io.Writer) error {
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			_, err := fmt.Fprint(stdout, credentialUsage)
			return err
		default:
			return fmt.Errorf("unknown argument %q", arg)
		}
	}

	socketPath := os.Getenv("Q8S_WORKLOAD_SOCKET")
	if socketPath == "" {
		socketPath = defaultWorkloadSocket
	}
	data, err := server.FetchWorkloadCredential(socketPath)
	if err != nil {
		return fmt.Errorf("q8s credential: %w", err)
	}
	_, err = stdout.Write(data)
	return err
}

const credentialUsage = `Usage: q8s credential

Prints the ExecCredential for the service account of the pod this command
runs in. q8s serve writes a kubeconfig that invokes it; running it outside a
q8s pod fails.

The socket is $Q8S_WORKLOAD_SOCKET, or /var/run/q8s/workload/sa.sock.
`
