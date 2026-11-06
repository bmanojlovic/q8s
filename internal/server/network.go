package server

import (
	"fmt"
	"sort"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"q8s/internal/podman"
	"q8s/internal/quadlet"
	"q8s/internal/systemd"
)

// networkOps is the small slice of podman networking q8s needs. It is an
// interface so tests (and any future non-CLI backend) never have to run
// podman; the real implementation is installed by New only when a systemd
// Manager exists, so Manager=nil test servers cannot reach the host.
type networkOps interface {
	EnsureNetwork(ns string) error
	NetworkSubnet(name string) (string, error)
	NetworkConnect(network, container, ip string) error
	NetworkDisconnect(network, container string) error
}

// podmanNetworkOps is the CLI-backed default. EnsureNetwork starts the
// namespace's quadlet unit: the q8s-<ns>.network file only creates the
// podman network when its generated service actually runs.
type podmanNetworkOps struct {
	manager *systemd.Manager
}

func (p podmanNetworkOps) EnsureNetwork(ns string) error {
	if p.manager == nil {
		return fmt.Errorf("systemd manager unavailable")
	}
	return p.manager.StartUnit(fmt.Sprintf("q8s-%s-network.service", ns))
}

func (p podmanNetworkOps) NetworkSubnet(name string) (string, error) {
	return podman.NetworkSubnet(name)
}

func (p podmanNetworkOps) NetworkConnect(network, container, ip string) error {
	return podman.NetworkConnect(network, container, ip)
}

func (p podmanNetworkOps) NetworkDisconnect(network, container string) error {
	return podman.NetworkDisconnect(network, container)
}

// networkState holds the ops plus the per-namespace subnet cache. The cache
// is only an optimization: a failed lookup is not cached, so a network that
// is still being created gets retried on the next call.
type networkState struct {
	ops networkOps

	mu      sync.Mutex
	subnets map[string]string // namespace -> subnet of q8s-<namespace>
}

func (s *Server) ensureNetwork(ns string) error {
	ops := s.networkOps()
	if ops == nil {
		return fmt.Errorf("network operations unavailable")
	}
	return ops.EnsureNetwork(ns)
}

func (s *Server) networkOps() networkOps {
	if s.network == nil {
		return nil
	}
	return s.network.ops
}

// subnetForNamespace returns (and caches) the first IPv4 subnet of a
// namespace network.
func (s *Server) subnetForNamespace(ns string) (string, error) {
	if s.network == nil {
		return "", fmt.Errorf("network operations unavailable")
	}
	s.network.mu.Lock()
	if subnet, ok := s.network.subnets[ns]; ok {
		s.network.mu.Unlock()
		return subnet, nil
	}
	s.network.mu.Unlock()

	ops := s.network.ops
	if ops == nil {
		return "", fmt.Errorf("network operations unavailable")
	}
	subnet, err := ops.NetworkSubnet(fmt.Sprintf("q8s-%s", ns))
	if err != nil {
		return "", err
	}
	s.network.mu.Lock()
	if s.network.subnets == nil {
		s.network.subnets = make(map[string]string)
	}
	s.network.subnets[ns] = subnet
	s.network.mu.Unlock()
	return subnet, nil
}

// dropSubnetCache forgets a deleted namespace's network, so a later
// namespace of the same name does not inherit the old subnet.
func (s *Server) dropSubnetCache(ns string) {
	if s.network == nil {
		return
	}
	s.network.mu.Lock()
	delete(s.network.subnets, ns)
	s.network.mu.Unlock()
}

// attachAllNetworks reports whether the pod opted in to every namespace
// network. This is the only path that is allowed to join another
// namespace's network, so callers must check it instead of inlining the
// annotation lookup.
func attachAllNetworks(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	v, ok := pod.Annotations[annAttachAllNetworks]
	if !ok {
		return false
	}
	attach, err := strconv.ParseBool(v)
	return err == nil && attach
}

// networkAttachments renders the namespace-network list for a pod. Ordinary
// pods get nil, which keeps today's single own-namespace network. An
// attach-all pod gets one entry per namespace (its own included), pinned to
// the last usable IPv4 of that namespace's subnet. A subnet that cannot be
// looked up yields an empty IP: the Network= line is still rendered so a
// restart recovers the attachment, and podman then assigns the address.
func (s *Server) networkAttachments(pod *corev1.Pod) []quadlet.NetworkAttachment {
	if pod == nil || pod.Spec.HostNetwork || !attachAllNetworks(pod) {
		return nil
	}
	namespaces := s.config.Store.Namespaces()
	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })
	attachments := make([]quadlet.NetworkAttachment, 0, len(namespaces))
	for _, ns := range namespaces {
		attachments = append(attachments, s.namespaceAttachment(ns.Name))
	}
	return attachments
}

// namespaceAttachment pins one namespace network. An unresolvable subnet
// leaves the IP empty; the Network= line is still useful for recovery and
// podman then picks an address.
func (s *Server) namespaceAttachment(ns string) quadlet.NetworkAttachment {
	ip := ""
	if subnet, err := s.subnetForNamespace(ns); err == nil {
		if last, err := quadlet.LastUsableIPv4(subnet); err == nil {
			ip = last
		}
	}
	return quadlet.NetworkAttachment{Namespace: ns, IP: ip}
}

// networkAttachmentFor returns the attachment of one namespace, if present.
func networkAttachmentFor(attachments []quadlet.NetworkAttachment, ns string) (quadlet.NetworkAttachment, bool) {
	for _, a := range attachments {
		if a.Namespace == ns {
			return a, true
		}
	}
	return quadlet.NetworkAttachment{}, false
}

// connectAttachAllPod attaches one pod to one namespace network. The
// container name is the one the quadlet renderer uses: <namespace>-<pod>
// for the single-container pods that attach-all allows.
func (s *Server) connectAttachAllPod(pod *corev1.Pod, ns string) {
	ops := s.networkOps()
	if ops == nil {
		return
	}
	attachments := s.networkAttachments(pod)
	attachment, ok := networkAttachmentFor(attachments, ns)
	if !ok {
		return
	}
	container := s.podContainerName(pod.Namespace, pod.Name, "")
	if err := ops.NetworkConnect(fmt.Sprintf("q8s-%s", ns), container, attachment.IP); err != nil {
		fmt.Printf("network connect %s/%s to q8s-%s: %v\n", pod.Namespace, pod.Name, ns, err)
	}
}

// refreshPodQuadlets rewrites a pod's quadlet files without touching its
// service: network reconfiguration must not restart a running pod. The
// daemon-reload is still needed so a later restart sees the new Network=
// lines.
func (s *Server) refreshPodQuadlets(pod *corev1.Pod, name string) {
	if s.config.QuadletDir == "" {
		return
	}
	pod = s.applyNodePorts(pod)
	resolved, envFile, err := s.resolveEnvFrom(pod)
	if err != nil {
		fmt.Printf("refresh pod quadlets %s/%s: %v\n", pod.Namespace, name, err)
		return
	}
	files, err := podQuadletFiles(name, resolved, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, resolved.Labels), s.podPVCMap(pod.Namespace, pod.Spec), envFile, s.networkAttachments(resolved))
	if err != nil {
		fmt.Printf("refresh pod quadlets %s/%s: %v\n", pod.Namespace, name, err)
		return
	}
	changed, err := writeQuadletFilesTracked(s.config.QuadletDir, files)
	if err != nil {
		fmt.Printf("refresh pod quadlets %s/%s: %v\n", pod.Namespace, name, err)
		return
	}
	removed := s.removeStaleOwnedUnits(s.config.QuadletDir, pod.Namespace, name, filenameSet(files))
	if len(changed) == 0 && !removed {
		return
	}
	if mgr := s.config.Manager; mgr != nil {
		if err := mgr.DaemonReload(); err != nil {
			fmt.Printf("daemon-reload after refreshing %s/%s: %v\n", pod.Namespace, name, err)
		}
	}
}

// attachNamespaceNetwork is the namespace-create side: make the network
// exist, then join every opted-in pod to it without a restart. Each pod's
// quadlet is re-rendered first so a restart or bare-quadlet recovery also
// knows about the new Network= line.
func (s *Server) attachNamespaceNetwork(ns string) {
	if err := s.ensureNetwork(ns); err != nil {
		fmt.Printf("ensure network q8s-%s: %v\n", ns, err)
	}
	for _, pod := range s.config.Store.AllPods() {
		if pod.Spec.HostNetwork || !attachAllNetworks(pod) {
			continue
		}
		s.refreshPodQuadlets(pod, pod.Name)
		s.connectAttachAllPod(pod, ns)
	}
}

// detachNamespaceNetwork is the namespace-delete side. It must run before
// the network unit is removed: podman refuses to delete a network that
// still has containers attached.
func (s *Server) detachNamespaceNetwork(ns string) {
	ops := s.networkOps()
	for _, pod := range s.config.Store.AllPods() {
		if pod.Spec.HostNetwork || !attachAllNetworks(pod) {
			continue
		}
		if ops != nil {
			container := s.podContainerName(pod.Namespace, pod.Name, "")
			if err := ops.NetworkDisconnect(fmt.Sprintf("q8s-%s", ns), container); err != nil {
				fmt.Printf("network disconnect %s/%s from q8s-%s: %v\n", pod.Namespace, pod.Name, ns, err)
			}
		}
		s.refreshPodQuadlets(pod, pod.Name)
	}
	s.dropSubnetCache(ns)
}

// ReconcileNetworkAttachments re-joins every attach-all pod to every
// namespace network on startup. It is idempotent: already connected and
// already absent are both success in the podman layer. Errors are logged,
// never fatal, so one bad network does not keep the API server from
// starting.
func (s *Server) ReconcileNetworkAttachments() {
	ops := s.networkOps()
	if ops == nil {
		return
	}
	namespaces := s.config.Store.Namespaces()
	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })
	// Every network exists (and its subnet is known) before any quadlet is
	// re-rendered: a quadlet regenerated earlier in startup, before its
	// networks existed, carries Network= lines without the fixed IP.
	ready := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		if err := s.ensureNetwork(ns.Name); err != nil {
			fmt.Printf("ensure network q8s-%s: %v\n", ns.Name, err)
			continue
		}
		ready[ns.Name] = true
	}
	for _, pod := range s.config.Store.AllPods() {
		if pod.Spec.HostNetwork || !attachAllNetworks(pod) {
			continue
		}
		s.refreshPodQuadlets(pod, pod.Name)
		container := s.podContainerName(pod.Namespace, pod.Name, "")
		for _, ns := range namespaces {
			if !ready[ns.Name] {
				continue
			}
			attachment := s.namespaceAttachment(ns.Name)
			if err := ops.NetworkConnect(fmt.Sprintf("q8s-%s", ns.Name), container, attachment.IP); err != nil {
				fmt.Printf("network connect %s/%s to q8s-%s: %v\n", pod.Namespace, pod.Name, ns.Name, err)
			}
		}
	}
}
