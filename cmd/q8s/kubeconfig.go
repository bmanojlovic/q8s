package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"q8s/pkg/install"
)

// kubeconfigOptions is the parsed command line of `q8s kubeconfig`.
type kubeconfigOptions struct {
	user            string
	userSet         bool
	groups          []string
	days            int
	daysSet         bool
	revokeUser      string
	revokeUserSet   bool
	revokeSerial    string
	revokeSerialSet bool
	listRevoked     bool
	help            bool
}

// cmdKubeconfig prints a kubeconfig or manages the revocation list.
func cmdKubeconfig() {
	d := resolveDirs(os.Getuid() == 0)
	if err := runKubeconfig(os.Args[2:], d, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runKubeconfig is cmdKubeconfig without the process exit, so tests can
// exercise the command.
func runKubeconfig(args []string, d dirs, stdout, stderr io.Writer) error {
	opts, err := parseKubeconfigArgs(args)
	if err != nil {
		return err
	}
	if opts.help {
		_, err := fmt.Fprint(stdout, kubeconfigUsage)
		return err
	}

	switch {
	case opts.listRevoked:
		return printRevoked(d, stdout)
	case opts.revokeUser != "":
		return revokeUser(d, opts.revokeUser, stderr)
	case opts.revokeSerial != "":
		return revokeSerial(d, opts.revokeSerial, stderr)
	}

	certDir := filepath.Join(d.dataDir, "certs")
	clusterName := "q8s"
	serverURL := fmt.Sprintf("https://localhost:%d", resolvePort(d))
	if cfg, err := install.LoadConfig(d.dataDir); err == nil {
		if cfg.ServerURL != "" {
			serverURL = cfg.ServerURL
		}
		if cfg.Name != "" {
			clusterName = cfg.Name
		}
	}

	if opts.user == "" {
		return printAdminKubeconfig(certDir, clusterName, serverURL, stdout)
	}

	caCert, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", filepath.Join(certDir, "ca.crt"), err)
	}
	caKey, err := os.ReadFile(filepath.Join(certDir, "ca.key"))
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", filepath.Join(certDir, "ca.key"), err)
	}

	issued, err := install.IssueClientCertificate(caCert, caKey, opts.user, opts.groups, opts.days)
	if err != nil {
		return err
	}
	for _, group := range opts.groups {
		if group == "system:masters" || group == "q8s-user" {
			fmt.Fprintf(stderr, "warning: group %q grants full administrator access; this kubeconfig can do anything\n", group)
		}
	}
	fmt.Fprintf(stderr, "certificate serial: %s\n", install.SerialHex(issued.Serial))

	identity := opts.user + "@" + clusterName
	data, err := renderUserKubeconfig(clusterName, serverURL, caCert, issued.CertPEM, issued.KeyPEM, identity)
	if err != nil {
		return err
	}
	_, err = stdout.Write(data)
	return err
}

func parseKubeconfigArgs(args []string) (kubeconfigOptions, error) {
	opts := kubeconfigOptions{days: install.DefaultClientCertDays}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		flagName := arg
		inlineValue := ""
		hasInlineValue := false
		if j := strings.IndexByte(arg, '='); j >= 0 {
			flagName = arg[:j]
			inlineValue = arg[j+1:]
			hasInlineValue = true
		}
		nextValue := func(flag string) (string, error) {
			if hasInlineValue {
				return inlineValue, nil
			}
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s requires a value", flag)
			}
			return args[i], nil
		}

		switch flagName {
		case "-h", "--help":
			if hasInlineValue {
				return opts, fmt.Errorf("--help does not take a value")
			}
			opts.help = true
			return opts, nil
		case "--user":
			value, err := nextValue(flagName)
			if err != nil {
				return opts, err
			}
			opts.user = value
			opts.userSet = true
		case "--group":
			value, err := nextValue(flagName)
			if err != nil {
				return opts, err
			}
			opts.groups = append(opts.groups, value)
		case "--days":
			value, err := nextValue(flagName)
			if err != nil {
				return opts, err
			}
			days, err := strconv.Atoi(value)
			if err != nil {
				return opts, fmt.Errorf("--days requires an integer, got %q", value)
			}
			opts.days = days
			opts.daysSet = true
		case "--revoke":
			value, err := nextValue(flagName)
			if err != nil {
				return opts, err
			}
			opts.revokeUser = value
			opts.revokeUserSet = true
		case "--revoke-serial":
			value, err := nextValue(flagName)
			if err != nil {
				return opts, err
			}
			opts.revokeSerial = value
			opts.revokeSerialSet = true
		case "--revoked":
			if hasInlineValue {
				return opts, fmt.Errorf("--revoked does not take a value")
			}
			opts.listRevoked = true
		default:
			return opts, fmt.Errorf("unknown argument %q", arg)
		}
	}

	actions := 0
	if opts.listRevoked {
		actions++
	}
	if opts.revokeUserSet {
		actions++
	}
	if opts.revokeSerialSet {
		actions++
	}
	if actions > 1 {
		return opts, fmt.Errorf("--revoke, --revoke-serial and --revoked are mutually exclusive")
	}
	if actions > 0 {
		if opts.userSet || len(opts.groups) > 0 || opts.daysSet {
			return opts, fmt.Errorf("--user, --group and --days cannot be combined with --revoke, --revoke-serial or --revoked")
		}
		if opts.revokeUserSet && opts.revokeUser == "" {
			return opts, fmt.Errorf("--revoke requires a value")
		}
		if opts.revokeSerialSet && opts.revokeSerial == "" {
			return opts, fmt.Errorf("--revoke-serial requires a value")
		}
		return opts, nil
	}

	if !opts.userSet {
		if len(opts.groups) > 0 {
			return opts, fmt.Errorf("--group requires --user")
		}
		if opts.daysSet {
			return opts, fmt.Errorf("--days requires --user")
		}
		return opts, nil
	}
	if err := install.ValidateIdentity("user name", opts.user); err != nil {
		return opts, err
	}
	for _, group := range opts.groups {
		if err := install.ValidateIdentity("group", group); err != nil {
			return opts, err
		}
	}
	if err := install.ValidateClientCertDays(opts.days); err != nil {
		return opts, err
	}
	return opts, nil
}

func readBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// printAdminKubeconfig prints the kubeconfig `q8s install` has always
// produced. Its output is byte for byte unchanged.
func printAdminKubeconfig(certDir, clusterName, serverURL string, stdout io.Writer) error {
	ca, err := readBase64(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		return err
	}
	cert, err := readBase64(filepath.Join(certDir, "client.crt"))
	if err != nil {
		return err
	}
	key, err := readBase64(filepath.Join(certDir, "client.key"))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s
  name: %s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
current-context: %s
preferences: {}
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
`, ca, serverURL, clusterName, clusterName, clusterName, clusterName, clusterName, clusterName, cert, key)
	return err
}

const kubeconfigUsage = `Usage: q8s kubeconfig [flags]

Without --user, prints the administrator kubeconfig created by q8s install.

Flags:
  --user NAME            Issue a client certificate and kubeconfig for NAME
  --group GROUP          Add GROUP to the certificate; repeat for several groups
  --days N               Certificate lifetime in days (default 365, max 3650)
  --revoke NAME          Refuse future requests from certificate user NAME
  --revoke-serial HEX    Refuse future requests from certificate serial HEX
  --revoked              List revoked users and serials
`

// kubeconfig is the subset of the client configuration file q8s emits.
type kubeconfig struct {
	APIVersion     string                 `json:"apiVersion"`
	Kind           string                 `json:"kind"`
	Clusters       []kubeconfigCluster    `json:"clusters"`
	Contexts       []kubeconfigContext    `json:"contexts"`
	CurrentContext string                 `json:"current-context"`
	Preferences    map[string]interface{} `json:"preferences"`
	Users          []kubeconfigUser       `json:"users"`
}

type kubeconfigCluster struct {
	Name    string                `json:"name"`
	Cluster kubeconfigClusterData `json:"cluster"`
}

type kubeconfigClusterData struct {
	CertificateAuthorityData string `json:"certificate-authority-data"`
	Server                   string `json:"server"`
}

type kubeconfigContext struct {
	Name    string                `json:"name"`
	Context kubeconfigContextData `json:"context"`
}

type kubeconfigContextData struct {
	Cluster string `json:"cluster"`
	User    string `json:"user"`
}

type kubeconfigUser struct {
	Name string             `json:"name"`
	User kubeconfigUserData `json:"user"`
}

type kubeconfigUserData struct {
	ClientCertificateData string `json:"client-certificate-data"`
	ClientKeyData         string `json:"client-key-data"`
}

// renderUserKubeconfig builds a kubeconfig whose cluster is the q8s cluster
// and whose user and context are both named identity (NAME@CLUSTER).
func renderUserKubeconfig(clusterName, serverURL string, caCert, clientCert, clientKey []byte, identity string) ([]byte, error) {
	cfg := kubeconfig{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []kubeconfigCluster{{
			Name: clusterName,
			Cluster: kubeconfigClusterData{
				CertificateAuthorityData: base64.StdEncoding.EncodeToString(caCert),
				Server:                   serverURL,
			},
		}},
		Contexts: []kubeconfigContext{{
			Name: identity,
			Context: kubeconfigContextData{
				Cluster: clusterName,
				User:    identity,
			},
		}},
		CurrentContext: identity,
		Preferences:    map[string]interface{}{},
		Users: []kubeconfigUser{{
			Name: identity,
			User: kubeconfigUserData{
				ClientCertificateData: base64.StdEncoding.EncodeToString(clientCert),
				ClientKeyData:         base64.StdEncoding.EncodeToString(clientKey),
			},
		}},
	}
	return yaml.Marshal(cfg)
}

func revokedFilePath(d dirs) string {
	return install.RevokedPath(d.dataDir)
}

func printRevoked(d dirs, stdout io.Writer) error {
	revoked, err := install.LoadRevokedCertificates(revokedFilePath(d))
	if err != nil {
		return err
	}
	if len(revoked.Users) == 0 && len(revoked.Serials) == 0 {
		_, err = fmt.Fprintln(stdout, "no revoked users or serials")
		return err
	}
	for _, user := range revoked.Users {
		if _, err := fmt.Fprintf(stdout, "user: %s\n", user); err != nil {
			return err
		}
	}
	for _, serial := range revoked.Serials {
		if _, err := fmt.Fprintf(stdout, "serial: %s\n", serial); err != nil {
			return err
		}
	}
	return nil
}

func revokeUser(d dirs, userName string, stderr io.Writer) error {
	if userName == "q8s-user" {
		return fmt.Errorf("refusing to revoke user %q: it is the administrator identity issued by q8s install", userName)
	}
	if err := install.ValidateIdentity("user name", userName); err != nil {
		return err
	}
	revoked, err := install.LoadRevokedCertificates(revokedFilePath(d))
	if err != nil {
		return err
	}
	if !revoked.HasUser(userName) {
		revoked.Users = append(revoked.Users, userName)
	}
	if err := install.SaveRevokedCertificates(revokedFilePath(d), revoked); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stderr, "revoked user %q\n", userName)
	return err
}

func revokeSerial(d dirs, serial string, stderr io.Writer) error {
	normalized := install.NormalizeSerial(serial)
	if normalized == "" {
		return fmt.Errorf("--revoke-serial requires a hexadecimal serial number")
	}
	for _, r := range normalized {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("--revoke-serial requires a hexadecimal serial number, got %q", serial)
		}
	}
	revoked, err := install.LoadRevokedCertificates(revokedFilePath(d))
	if err != nil {
		return err
	}
	if !revoked.HasSerial(normalized) {
		revoked.Serials = append(revoked.Serials, normalized)
	}
	if err := install.SaveRevokedCertificates(revokedFilePath(d), revoked); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stderr, "revoked serial %s\n", normalized)
	return err
}
