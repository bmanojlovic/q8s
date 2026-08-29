package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	jsonser "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"q8s/internal/quadlet"
)

var scheme = runtime.NewScheme()
var codecs serializer.CodecFactory

func init() {
	corev1.AddToScheme(scheme)
	appsv1.AddToScheme(scheme)
	batchv1.AddToScheme(scheme)
	networkingv1.AddToScheme(scheme)
	codecs = serializer.NewCodecFactory(scheme)
}

func encoder() *jsonser.Serializer {
	return jsonser.NewSerializer(jsonser.SimpleMetaFactory{}, scheme, scheme, false)
}

func decode(r *http.Request, obj runtime.Object) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "application/json"
	}
	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), ct)
	if !ok {
		info, _ = runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/json")
	}
	_, _, err = info.Serializer.Decode(body, nil, obj)
	return err
}

func encode(w http.ResponseWriter, obj runtime.Object, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	encoder().Encode(obj, w)
}

func (s *Server) respondStatus(w http.ResponseWriter, code int, reason string, format string, args ...interface{}) {
	encode(w, &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Failure",
		Message:  fmt.Sprintf(format, args...),
		Reason:   metav1.StatusReason(reason),
		Code:     int32(code),
	}, code)
}

func writeQuadletFile(dir, filename string, content []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(dir+"/"+filename, content, 0644)
}

func (s *Server) rv() string {
	return fmt.Sprintf("%d", s.config.Store.ResourceVersion())
}

func boolPtr(b bool) *bool { return &b }

// watchItem constrains a watch-eligible resource to a pointer type whose
// element is Item, satisfying both runtime.Object (for encoding) and
// metav1.Object (for reading name/namespace/resourceVersion when diffing).
type watchItem[Item any] interface {
	*Item
	runtime.Object
	metav1.Object
}

// respondList handles the table/list/watch response pattern common to all
// list endpoints. listFn is called once for a plain list or table response;
// for a watch request (?watch=true) it is called repeatedly to diff
// successive snapshots into ADDED/MODIFIED/DELETED events.
func respondList[Item any, PI watchItem[Item], List runtime.Object](
	w http.ResponseWriter,
	r *http.Request,
	s *Server,
	listFn func() []PI,
	toTable func([]PI, string) *table,
	makeList func([]Item) List,
) {
	// Applying the labelSelector here, once, covers every resource type and
	// both plain-list and watch (serveWatch calls listFn repeatedly, so the
	// filter re-applies on every snapshot) without each of the ~17 call
	// sites needing to know about it.
	if reqs := parseLabelSelector(r.URL.Query().Get("labelSelector")); len(reqs) > 0 {
		inner := listFn
		listFn = func() []PI {
			all := inner()
			filtered := make([]PI, 0, len(all))
			for _, item := range all {
				if matchesSelector(item.GetLabels(), reqs) {
					filtered = append(filtered, item)
				}
			}
			return filtered
		}
	}
	if r.URL.Query().Get("watch") == "true" {
		serveWatch(w, r, s, listFn)
		return
	}
	items := listFn()
	if isTableRequest(r) {
		encodeTable(w, toTable(items, s.rv()))
		return
	}
	plain := make([]Item, len(items))
	for i, p := range items {
		plain[i] = *p
	}
	encode(w, makeList(plain), http.StatusOK)
}

// serveWatch streams ADDED/MODIFIED/DELETED WatchEvents for a resource
// collection. q8s has no per-object event log, so it diffs successive
// snapshots taken each time Store.Changed() fires (any mutation, or the 5s
// sync-loop reconciliation) against the last-seen state per watcher. This is
// enough to satisfy clients that open watches for live updates (Freelens,
// k9s, client-go informers) even though it can't replay history from a
// specific resourceVersion the way a real apiserver's watch cache can.
//
// listFn returns pointers straight out of the Store's maps, not copies —
// same as every other read path in this file. write() must therefore only
// read from o, never mutate it (e.g. via GetObjectKind().Set...); doing so
// would race with concurrent requests touching the same stored object. Every
// create handler already sets APIVersion/Kind before storing, so encoding o
// as-is is enough.
func serveWatch[Item any, PI watchItem[Item]](w http.ResponseWriter, r *http.Request, s *Server, listFn func() []PI) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	key := func(o PI) string { return o.GetNamespace() + "/" + o.GetName() }

	type entry struct {
		rv  string
		obj PI
	}
	seen := make(map[string]entry)
	for _, o := range listFn() {
		seen[key(o)] = entry{rv: o.GetResourceVersion(), obj: o}
	}

	enc := json.NewEncoder(w)
	write := func(evType string, o PI) bool {
		raw, err := json.Marshal(o)
		if err != nil {
			return true
		}
		if err := enc.Encode(metav1.WatchEvent{Type: evType, Object: runtime.RawExtension{Raw: raw}}); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.config.Store.Changed():
		}

		current := listFn()
		currentKeys := make(map[string]bool, len(current))
		for _, o := range current {
			k := key(o)
			currentKeys[k] = true
			prev, existed := seen[k]
			if existed && prev.rv == o.GetResourceVersion() {
				continue
			}
			evType := "MODIFIED"
			if !existed {
				evType = "ADDED"
			}
			if !write(evType, o) {
				return
			}
			seen[k] = entry{rv: o.GetResourceVersion(), obj: o}
		}
		for k, e := range seen {
			if !currentKeys[k] {
				if !write("DELETED", e.obj) {
					return
				}
				delete(seen, k)
			}
		}
	}
}

// deployUnit writes a quadlet file, daemon-reloads, then starts unitName (if non-empty).
func (s *Server) deployUnit(dir, filename string, content []byte, unitName string) {
	if err := writeQuadletFile(dir, filename, content); err != nil {
		fmt.Printf("write %s: %v\n", filename, err)
		return
	}
	mgr := s.config.Manager
	if mgr == nil {
		return
	}
	if err := mgr.DaemonReload(); err != nil {
		fmt.Printf("daemon-reload: %v\n", err)
		return
	}
	if unitName != "" {
		if err := mgr.StartUnit(unitName); err != nil {
			fmt.Printf("start %s: %v\n", unitName, err)
		}
	}
}

// reloadAfterRemove deletes paths then triggers a daemon-reload.
func (s *Server) reloadAfterRemove(paths ...string) {
	for _, p := range paths {
		os.Remove(p)
	}
	if mgr := s.config.Manager; mgr != nil {
		mgr.DaemonReload()
	}
}

// --- Namespaced router ---

func (s *Server) handleNamespaced(w http.ResponseWriter, r *http.Request) {
	ns, resource, name, ok := parseNamespaceResource(r.URL.Path)
	if !ok {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", "invalid namespace resource path")
		return
	}

	if resource == "" {
		switch r.Method {
		case http.MethodGet:
			s.handleNamespaceGet(w, r, ns)
		case http.MethodDelete:
			s.handleNamespaceDelete(w, r, ns)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	switch resource {
	case "pods":
		s.handlePods(w, r, ns, name)
	case "services":
		s.handleServices(w, r, ns, name)
	case "persistentvolumeclaims":
		s.handlePVCs(w, r, ns, name)
	case "configmaps":
		s.handleConfigMaps(w, r, ns, name)
	case "secrets":
		s.handleSecrets(w, r, ns, name)
	case "events":
		s.handleEvents(w, r, ns)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

// --- Pods ---

func (s *Server) handlePods(w http.ResponseWriter, r *http.Request, ns, name string) {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		podName, sub := name[:i], name[i+1:]
		switch sub {
		case "log":
			s.handlePodLogs(w, r, ns, podName)
		case "exec":
			s.handlePodExec(w, r, ns, podName)
		default:
			s.respondStatus(w, http.StatusNotFound, "NotFound", "subresource %q not supported", sub)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*corev1.Pod { return s.config.Store.Pods(ns) }, podsToTable,
				func(items []corev1.Pod) *corev1.PodList {
					return &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: items}
				})
		} else {
			pod, err := s.config.Store.GetPod(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, podsToTable([]*corev1.Pod{pod}, s.rv()))
				return
			}
			encode(w, pod, http.StatusOK)
		}
	case http.MethodPost:
		var pod corev1.Pod
		if err := decode(r, &pod); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		pod.APIVersion = "v1"
		pod.Kind = "Pod"
		if pod.Namespace == "" {
			pod.Namespace = ns
		}
		if err := validateName("namespace", pod.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", pod.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		// Fail fast with a proper 400 instead of storing the pod and only
		// discovering at quadlet-generation time (after the fact, logged
		// server-side only) that its spec can't be safely turned into a
		// unit file — e.g. an image reference or env value containing
		// characters that would corrupt the generated quadlet.
		if _, err := quadlet.Container(pod.Name, &pod, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, pod.Labels), s.podPVCMap(pod.Namespace, pod.Spec)); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		// hostPort and Service socket units both bind the host — reject if
		// a Service already owns a port this pod wants as hostPort.
		if port, conflict := s.podHostPortConflict(&pod); conflict {
			s.respondStatus(w, http.StatusConflict, "Conflict",
				"hostPort %d conflicts with an existing Service port — use one or the other", port)
			return
		}
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		// Set initial status before storing: the store hands the pointer to
		// any concurrent reader (watch, other requests) the moment it's
		// inserted, so mutating fields on it afterwards would race.
		pod.Status.Phase = corev1.PodPending
		pod.Status.StartTime = &metav1.Time{Time: time.Now()}
		created, err := s.config.Store.CreatePod(&pod)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generatePodQuadlet(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		pod, err := s.config.Store.GetPod(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(pod)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched corev1.Pod
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdatePod(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.redeployPodQuadlet(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		pod, err := s.config.Store.GetPod(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.stopPodUnit(pod)
		s.removePodQuadlet(pod)
		if err := s.config.Store.DeletePod(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   "Success",
			Message:  name,
			Reason:   metav1.StatusReason(name),
			Code:     http.StatusOK,
		}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePodLogs(w http.ResponseWriter, r *http.Request, ns, name string) {
	containerName := fmt.Sprintf("%s-%s", ns, name)
	q := r.URL.Query()

	args := []string{"logs"}
	follow := q.Get("follow") == "true"
	if follow {
		args = append(args, "--follow")
	}
	if tail := q.Get("tailLines"); tail != "" {
		if _, err := strconv.Atoi(tail); err == nil {
			args = append(args, "--tail", tail)
		}
	}
	if q.Get("timestamps") == "true" {
		args = append(args, "--timestamps")
	}
	args = append(args, containerName)

	if !follow {
		out, err := exec.CommandContext(r.Context(), "podman", args...).CombinedOutput()
		if err != nil && len(out) == 0 {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "podman logs: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(out)
		return
	}

	cmd := exec.CommandContext(r.Context(), "podman", args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		s.respondStatus(w, http.StatusInternalServerError, "InternalError", "podman logs: %v", err)
		return
	}
	go func() {
		cmd.Wait()
		pw.Close()
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// --- Services ---

// matchingServiceAliases returns the names of every Service in ns whose
// equality selector matches labels. Each becomes a Podman NetworkAlias on
// the backing container (see quadlet.Container), so aardvark-dns resolves
// the Service name straight to it — no separate Service→pod bookkeeping.
func (s *Server) matchingServiceAliases(ns string, labels map[string]string) []string {
	var aliases []string
	for _, svc := range s.config.Store.Services(ns) {
		if matchesEqualitySelector(labels, svc.Spec.Selector) {
			aliases = append(aliases, svc.Name)
		}
	}
	return aliases
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*corev1.Service { return s.config.Store.Services(ns) }, svcsToTable,
				func(items []corev1.Service) *corev1.ServiceList {
					return &corev1.ServiceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceList"}, Items: items}
				})
		} else {
			svc, err := s.config.Store.GetService(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, svcsToTable([]*corev1.Service{svc}, s.rv()))
				return
			}
			encode(w, svc, http.StatusOK)
		}
	case http.MethodPost:
		var svc corev1.Service
		if err := decode(r, &svc); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		svc.APIVersion = "v1"
		svc.Kind = "Service"
		if svc.Namespace == "" {
			svc.Namespace = ns
		}
		if err := validateName("namespace", svc.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", svc.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateService(&svc)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		if port, conflict := s.serviceHostPortConflict(created); conflict {
			s.config.Store.DeleteService(ns, created.Name)
			s.respondStatus(w, http.StatusConflict, "Conflict",
				"service port %d conflicts with hostPort on a matching pod — use one or the other", port)
			return
		}
		s.generateServiceSocket(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		svc, err := s.config.Store.GetService(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(svc)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched corev1.Service
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateService(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		if port, conflict := s.serviceHostPortConflict(updated); conflict {
			s.respondStatus(w, http.StatusConflict, "Conflict",
				"service port %d conflicts with hostPort on a matching pod — use one or the other", port)
			return
		}
		s.generateServiceSocket(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if err := s.config.Store.DeleteService(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- PVCs ---

func (s *Server) handlePVCs(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*corev1.PersistentVolumeClaim { return s.config.Store.PVCs(ns) }, pvcsToTable,
				func(items []corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaimList {
					return &corev1.PersistentVolumeClaimList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"}, Items: items}
				})
		} else {
			pvc, err := s.config.Store.GetPVC(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, pvcsToTable([]*corev1.PersistentVolumeClaim{pvc}, s.rv()))
				return
			}
			encode(w, pvc, http.StatusOK)
		}
	case http.MethodPost:
		var pvc corev1.PersistentVolumeClaim
		if err := decode(r, &pvc); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		pvc.APIVersion = "v1"
		pvc.Kind = "PersistentVolumeClaim"
		if pvc.Namespace == "" {
			pvc.Namespace = ns
		}
		if err := validateName("namespace", pvc.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", pvc.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreatePVC(&pvc)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generatePVCVolume(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		pvc, err := s.config.Store.GetPVC(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(pvc)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched corev1.PersistentVolumeClaim
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdatePVC(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		pvc, _ := s.config.Store.GetPVC(ns, name)
		if err := s.config.Store.DeletePVC(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if pvc != nil {
			s.removePVCVolume(pvc)
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- ConfigMaps ---

func (s *Server) handleConfigMaps(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*corev1.ConfigMap { return s.config.Store.ConfigMaps(ns) }, configMapsToTable,
				func(items []corev1.ConfigMap) *corev1.ConfigMapList {
					return &corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, Items: items}
				})
		} else {
			cm, err := s.config.Store.GetConfigMap(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, configMapsToTable([]*corev1.ConfigMap{cm}, s.rv()))
				return
			}
			encode(w, cm, http.StatusOK)
		}
	case http.MethodPost:
		var cm corev1.ConfigMap
		if err := decode(r, &cm); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		cm.APIVersion = "v1"
		cm.Kind = "ConfigMap"
		if cm.Namespace == "" {
			cm.Namespace = ns
		}
		if err := validateName("namespace", cm.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", cm.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateDataKeys(cm.Data, cm.BinaryData); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateConfigMap(&cm)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.writeConfigMapFiles(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPut:
		var cm corev1.ConfigMap
		if err := decode(r, &cm); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		cm.APIVersion = "v1"
		cm.Kind = "ConfigMap"
		if cm.Namespace == "" {
			cm.Namespace = ns
		}
		if cm.Name == "" {
			cm.Name = name
		}
		if err := validateName("namespace", cm.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", cm.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateDataKeys(cm.Data, cm.BinaryData); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateConfigMap(&cm)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.writeConfigMapFiles(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodPatch:
		cm, err := s.config.Store.GetConfigMap(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(cm)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched corev1.ConfigMap
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validateDataKeys(patched.Data, patched.BinaryData); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateConfigMap(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.writeConfigMapFiles(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		cm, _ := s.config.Store.GetConfigMap(ns, name)
		if err := s.config.Store.DeleteConfigMap(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if cm != nil {
			s.removeConfigMapFiles(cm)
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Secrets ---

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*corev1.Secret { return s.config.Store.Secrets(ns) }, secretsToTable,
				func(items []corev1.Secret) *corev1.SecretList {
					return &corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}, Items: items}
				})
		} else {
			secret, err := s.config.Store.GetSecret(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, secretsToTable([]*corev1.Secret{secret}, s.rv()))
				return
			}
			encode(w, secret, http.StatusOK)
		}
	case http.MethodPost:
		var secret corev1.Secret
		if err := decode(r, &secret); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		secret.APIVersion = "v1"
		secret.Kind = "Secret"
		if secret.Namespace == "" {
			secret.Namespace = ns
		}
		if err := validateName("namespace", secret.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", secret.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateDataKeys(secret.StringData, secret.Data); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateSecret(&secret)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.writeSecretFiles(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		sec, err := s.config.Store.GetSecret(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(sec)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
			// RFC 6902 JSON Patch — an array of ops (Terraform's provider
			// patches Secret data keys this way, since merge patches cannot
			// delete map entries).
			var ops []map[string]interface{}
			if err := json.Unmarshal(body, &ops); err != nil {
				s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
				return
			}
			if err := applyJSONPatch(base, ops); err != nil {
				s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
				return
			}
		} else {
			var overlay map[string]interface{}
			if err := json.Unmarshal(body, &overlay); err != nil {
				s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
				return
			}
			jsonMerge(base, overlay)
		}
		merged, _ := json.Marshal(base)
		var patched corev1.Secret
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validateDataKeys(patched.StringData, patched.Data); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateSecret(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.removeStaleSecretFiles(updated)
		s.writeSecretFiles(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		sec, _ := s.config.Store.GetSecret(ns, name)
		if err := s.config.Store.DeleteSecret(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		if sec != nil {
			s.removeSecretFiles(sec)
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Events ---

// handleEvents is GET/watch-only: events are synthetic, generated by the
// store as a side effect of other resources' lifecycle, never created,
// patched, or deleted directly by a client.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, ns string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	respondList(w, r, s, func() []*corev1.Event { return s.config.Store.Events(ns) }, eventsToTable,
		func(items []corev1.Event) *corev1.EventList {
			return &corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}, Items: items}
		})
}

// --- apps/v1 ---

func (s *Server) handleAppsNamespaced(w http.ResponseWriter, r *http.Request) {
	parts := strings.TrimPrefix(r.URL.Path, "/apis/apps/v1/namespaces/")
	parts = strings.TrimSuffix(parts, "/")
	pieces := strings.SplitN(parts, "/", 3)
	if len(pieces) < 2 {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "invalid apps namespace resource path")
		return
	}
	ns, resource := pieces[0], pieces[1]
	name := ""
	if len(pieces) == 3 {
		name = pieces[2]
	}
	switch resource {
	case "deployments":
		s.handleDeployments(w, r, ns, name)
	case "daemonsets", "statefulsets", "replicasets":
		s.handleAppsStub(w, r, resource)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

func (s *Server) handleAppsClusterList(w http.ResponseWriter, r *http.Request, resource string) {
	switch resource {
	case "deployments":
		respondList(w, r, s, s.config.Store.AllDeployments, deploymentsToTable,
			func(items []appsv1.Deployment) *appsv1.DeploymentList {
				return &appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, Items: items}
			})
	case "daemonsets", "statefulsets", "replicasets":
		s.handleAppsStub(w, r, resource)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

// handleAppsStub serves daemonsets/statefulsets/replicasets as permanently
// empty, GET/watch-only collections — see the comment in server.go for why.
func (s *Server) handleAppsStub(w http.ResponseWriter, r *http.Request, resource string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch resource {
	case "daemonsets":
		respondList(w, r, s, func() []*appsv1.DaemonSet { return nil }, daemonsetsToTable,
			func(items []appsv1.DaemonSet) *appsv1.DaemonSetList {
				return &appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, Items: items}
			})
	case "statefulsets":
		respondList(w, r, s, func() []*appsv1.StatefulSet { return nil }, statefulsetsToTable,
			func(items []appsv1.StatefulSet) *appsv1.StatefulSetList {
				return &appsv1.StatefulSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSetList"}, Items: items}
			})
	case "replicasets":
		respondList(w, r, s, func() []*appsv1.ReplicaSet { return nil }, replicasetsToTable,
			func(items []appsv1.ReplicaSet) *appsv1.ReplicaSetList {
				return &appsv1.ReplicaSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSetList"}, Items: items}
			})
	}
}

func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request, ns, name string) {
	// Route subresources: deployments/{name}/scale
	if i := strings.IndexByte(name, '/'); i >= 0 {
		depName, sub := name[:i], name[i+1:]
		if sub == "scale" {
			s.handleDeploymentScale(w, r, ns, depName)
		} else {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "subresource %q not supported", sub)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*appsv1.Deployment { return s.config.Store.Deployments(ns) }, deploymentsToTable,
				func(items []appsv1.Deployment) *appsv1.DeploymentList {
					return &appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, Items: items}
				})
		} else {
			dep, err := s.config.Store.GetDeployment(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, deploymentsToTable([]*appsv1.Deployment{dep}, s.rv()))
				return
			}
			encode(w, dep, http.StatusOK)
		}
	case http.MethodPost:
		var dep appsv1.Deployment
		if err := decode(r, &dep); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		dep.APIVersion = "apps/v1"
		dep.Kind = "Deployment"
		if dep.Namespace == "" {
			dep.Namespace = ns
		}
		if err := validateName("namespace", dep.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", dep.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateDeployment(&dep)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generateDeploymentQuadlets(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		dep, err := s.config.Store.GetDeployment(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		oldR := deploymentReplicas(dep)
		// JSON-merge the patch into the existing deployment so annotations,
		// template changes (e.g. kubectl rollout restart), and replica changes
		// all get persisted correctly.
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(dep)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched appsv1.Deployment
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateDeployment(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		newR := deploymentReplicas(updated)
		if newR != oldR {
			s.scaleDeployment(updated, oldR, newR)
		} else {
			for i := int32(0); i < newR; i++ {
				s.redeployDeploymentInstanceQuadlet(updated, i)
			}
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		dep, err := s.config.Store.GetDeployment(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.stopDeploymentUnits(dep)
		s.removeDeploymentQuadlets(dep)
		s.deleteDeploymentPods(dep)
		if err := s.config.Store.DeleteDeployment(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleDeploymentScale(w http.ResponseWriter, r *http.Request, ns, name string) {
	dep, err := s.config.Store.GetDeployment(ns, name)
	if err != nil {
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
		return
	}
	scaleResp := func(dep *appsv1.Deployment) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"apiVersion": "autoscaling/v1",
			"kind":       "Scale",
			"metadata":   map[string]interface{}{"name": dep.Name, "namespace": dep.Namespace},
			"spec":       map[string]interface{}{"replicas": deploymentReplicas(dep)},
			"status":     map[string]interface{}{"replicas": dep.Status.ReadyReplicas},
		})
	}
	switch r.Method {
	case http.MethodGet:
		scaleResp(dep)
	case http.MethodPatch, http.MethodPut:
		oldR := deploymentReplicas(dep)
		body, _ := io.ReadAll(r.Body)
		var patch struct {
			Spec struct {
				Replicas *int32 `json:"replicas"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(body, &patch); err == nil && patch.Spec.Replicas != nil {
			dep.Spec.Replicas = patch.Spec.Replicas
		}
		updated, err := s.config.Store.UpdateDeployment(dep)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.scaleDeployment(updated, oldR, deploymentReplicas(updated))
		scaleResp(updated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Cluster-scoped list (kubectl get -A) ---

func (s *Server) handleClusterList(w http.ResponseWriter, r *http.Request, resource string) {
	switch resource {
	case "pods":
		respondList(w, r, s, s.config.Store.AllPods, podsToTable,
			func(items []corev1.Pod) *corev1.PodList {
				return &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: items}
			})
	case "services":
		respondList(w, r, s, s.config.Store.AllServices, svcsToTable,
			func(items []corev1.Service) *corev1.ServiceList {
				return &corev1.ServiceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceList"}, Items: items}
			})
	case "persistentvolumeclaims":
		respondList(w, r, s, s.config.Store.AllPVCs, pvcsToTable,
			func(items []corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaimList {
				return &corev1.PersistentVolumeClaimList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"}, Items: items}
			})
	case "configmaps":
		respondList(w, r, s, s.config.Store.AllConfigMaps, configMapsToTable,
			func(items []corev1.ConfigMap) *corev1.ConfigMapList {
				return &corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, Items: items}
			})
	case "secrets":
		respondList(w, r, s, s.config.Store.AllSecrets, secretsToTable,
			func(items []corev1.Secret) *corev1.SecretList {
				return &corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}, Items: items}
			})
	case "events":
		respondList(w, r, s, s.config.Store.AllEvents, eventsToTable,
			func(items []corev1.Event) *corev1.EventList {
				return &corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}, Items: items}
			})
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

// --- Namespaces ---

func (s *Server) handleNamespaceList(w http.ResponseWriter, r *http.Request) {
	respondList(w, r, s, s.config.Store.Namespaces, namespacesToTable,
		func(items []corev1.Namespace) *corev1.NamespaceList {
			return &corev1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"}, Items: items}
		})
}

func (s *Server) handleNamespaceGet(w http.ResponseWriter, r *http.Request, name string) {
	ns, err := s.config.Store.GetNamespace(name)
	if err != nil {
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
		return
	}
	if isTableRequest(r) {
		encodeTable(w, namespacesToTable([]*corev1.Namespace{ns}, s.rv()))
		return
	}
	encode(w, ns, http.StatusOK)
}

func (s *Server) handleNamespaceCreate(w http.ResponseWriter, r *http.Request) {
	var ns corev1.Namespace
	if err := decode(r, &ns); err != nil {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
		return
	}
	ns.APIVersion = "v1"
	ns.Kind = "Namespace"
	if err := validateName("name", ns.Name); err != nil {
		s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
		return
	}
	created, err := s.config.Store.CreateNamespace(&ns)
	if err != nil {
		s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
		return
	}
	s.generateNamespaceNetwork(created.Name)
	encode(w, created, http.StatusCreated)
}

func (s *Server) purgeNamespaceResources(ns string) {
	for _, pod := range s.config.Store.Pods(ns) {
		s.stopPodUnit(pod)
		s.removePodQuadlet(pod)
	}
	for _, dep := range s.config.Store.Deployments(ns) {
		s.stopDeploymentUnits(dep)
		s.removeDeploymentQuadlets(dep)
	}
	for _, job := range s.config.Store.Jobs(ns) {
		s.stopJobUnit(job)
		s.removeJobQuadlet(job)
	}
	for _, cj := range s.config.Store.CronJobs(ns) {
		s.removeCronJobQuadlets(cj)
	}
	for _, cm := range s.config.Store.ConfigMaps(ns) {
		s.removeConfigMapFiles(cm)
	}
	for _, sec := range s.config.Store.Secrets(ns) {
		s.removeSecretFiles(sec)
	}
	s.config.Store.PurgeNamespace(ns)
}

func (s *Server) handleNamespaceDelete(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := s.config.Store.GetNamespace(name); err != nil {
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
		return
	}
	s.purgeNamespaceResources(name)
	if err := s.config.Store.DeleteNamespace(name); err != nil {
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
		return
	}
	s.removeNamespaceNetwork(name)
	encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
}

// --- batch/v1 ---

func (s *Server) handleBatchNamespaced(w http.ResponseWriter, r *http.Request) {
	ns, resource, name, ok := parseBatchNamespaceResource(r.URL.Path)
	if !ok {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", "invalid batch namespace resource path")
		return
	}
	switch resource {
	case "jobs":
		s.handleJobs(w, r, ns, name)
	case "cronjobs":
		s.handleCronJobs(w, r, ns, name)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

func (s *Server) handleBatchClusterList(w http.ResponseWriter, r *http.Request, resource string) {
	switch resource {
	case "jobs":
		respondList(w, r, s, s.config.Store.AllJobs, jobsToTable,
			func(items []batchv1.Job) *batchv1.JobList {
				return &batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}, Items: items}
			})
	case "cronjobs":
		respondList(w, r, s, s.config.Store.AllCronJobs, cronJobsToTable,
			func(items []batchv1.CronJob) *batchv1.CronJobList {
				return &batchv1.CronJobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJobList"}, Items: items}
			})
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

// --- Jobs ---

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*batchv1.Job { return s.config.Store.Jobs(ns) }, jobsToTable,
				func(items []batchv1.Job) *batchv1.JobList {
					return &batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}, Items: items}
				})
		} else {
			job, err := s.config.Store.GetJob(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, jobsToTable([]*batchv1.Job{job}, s.rv()))
				return
			}
			encode(w, job, http.StatusOK)
		}
	case http.MethodPost:
		var job batchv1.Job
		if err := decode(r, &job); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		job.APIVersion = "batch/v1"
		job.Kind = "Job"
		if job.Namespace == "" {
			job.Namespace = ns
		}
		if err := validateName("namespace", job.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", job.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateJob(&job)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generateJobQuadlet(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		job, err := s.config.Store.GetJob(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(job)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched batchv1.Job
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateJob(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		if s.config.QuadletDir != "" {
			if content, err := quadlet.JobContainer(updated.Name, updated, s.config.ConfigDir, s.podPVCMap(updated.Namespace, updated.Spec.Template.Spec)); err == nil {
				writeQuadletFile(s.config.QuadletDir,
					fmt.Sprintf("%s-%s-job.container", updated.Namespace, updated.Name), content)
			}
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		job, err := s.config.Store.GetJob(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.stopJobUnit(job)
		s.removeJobQuadlet(job)
		if err := s.config.Store.DeleteJob(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- CronJobs ---

func (s *Server) handleCronJobs(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*batchv1.CronJob { return s.config.Store.CronJobs(ns) }, cronJobsToTable,
				func(items []batchv1.CronJob) *batchv1.CronJobList {
					return &batchv1.CronJobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJobList"}, Items: items}
				})
		} else {
			cj, err := s.config.Store.GetCronJob(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, cronJobsToTable([]*batchv1.CronJob{cj}, s.rv()))
				return
			}
			encode(w, cj, http.StatusOK)
		}
	case http.MethodPost:
		var cj batchv1.CronJob
		if err := decode(r, &cj); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		cj.APIVersion = "batch/v1"
		cj.Kind = "CronJob"
		if cj.Namespace == "" {
			cj.Namespace = ns
		}
		if err := validateName("namespace", cj.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", cj.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateCronJob(&cj)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generateCronJobQuadlets(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		cj, err := s.config.Store.GetCronJob(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(cj)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched batchv1.CronJob
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateCronJob(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.generateCronJobQuadlets(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		cj, err := s.config.Store.GetCronJob(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.removeCronJobQuadlets(cj)
		if err := s.config.Store.DeleteCronJob(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleNetworkingNamespaced(w http.ResponseWriter, r *http.Request) {
	ns, resource, name, ok := parseNetworkingNamespaceResource(r.URL.Path)
	if !ok {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", "invalid networking namespace resource path")
		return
	}
	switch resource {
	case "ingresses":
		s.handleIngresses(w, r, ns, name)
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

func (s *Server) handleNetworkingClusterList(w http.ResponseWriter, r *http.Request, resource string) {
	switch resource {
	case "ingresses":
		respondList(w, r, s, s.config.Store.AllIngresses, ingressesToTable,
			func(items []networkingv1.Ingress) *networkingv1.IngressList {
				return &networkingv1.IngressList{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "IngressList"}, Items: items}
			})
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

// --- Ingresses ---

func (s *Server) handleIngresses(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s, func() []*networkingv1.Ingress { return s.config.Store.Ingresses(ns) }, ingressesToTable,
				func(items []networkingv1.Ingress) *networkingv1.IngressList {
					return &networkingv1.IngressList{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "IngressList"}, Items: items}
				})
		} else {
			ing, err := s.config.Store.GetIngress(ns, name)
			if err != nil {
				s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
				return
			}
			if isTableRequest(r) {
				encodeTable(w, ingressesToTable([]*networkingv1.Ingress{ing}, s.rv()))
				return
			}
			encode(w, ing, http.StatusOK)
		}
	case http.MethodPost:
		var ing networkingv1.Ingress
		if err := decode(r, &ing); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		ing.APIVersion = "networking.k8s.io/v1"
		ing.Kind = "Ingress"
		if ing.Namespace == "" {
			ing.Namespace = ns
		}
		if err := validateName("namespace", ing.Namespace); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateName("name", ing.Name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateIngress(&ing); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		created, err := s.config.Store.CreateIngress(&ing)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generateTraefikConfig(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		ing, err := s.config.Store.GetIngress(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		existing, _ := json.Marshal(ing)
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched networkingv1.Ingress
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validateIngress(&patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateIngress(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.generateTraefikConfig(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if err := s.config.Store.DeleteIngress(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		s.removeTraefikConfig(ns, name)
		encode(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Traefik dynamic config ---

// generateTraefikConfig writes a Traefik dynamic config file for an Ingress.
// The file is placed in TraefikDir and picked up by Traefik's file provider.
func (s *Server) generateTraefikConfig(ing *networkingv1.Ingress) {
	if s.config.TraefikDir == "" {
		return
	}

	var routers, services strings.Builder
	idx := 0

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			routerName := fmt.Sprintf("%s-%s-%d", ing.Namespace, ing.Name, idx)
			idx++
			svcName := path.Backend.Service.Name
			svcPort := int32(80)
			if path.Backend.Service.Port.Number != 0 {
				svcPort = path.Backend.Service.Port.Number
			}

			// Resolve the actual host port from the Service object
			hostPort := svcPort
			if svc, err := s.config.Store.GetService(ing.Namespace, svcName); err == nil {
				for _, p := range svc.Spec.Ports {
					if p.Port == svcPort || p.Name == path.Backend.Service.Port.Name {
						if p.NodePort != 0 {
							hostPort = p.NodePort
						} else {
							hostPort = p.Port
						}
						break
					}
				}
			}

			// Build router rule
			var ruleParts []string
			if rule.Host != "" {
				ruleParts = append(ruleParts, fmt.Sprintf("Host(`%s`)", rule.Host))
			}
			if path.Path != "" && path.Path != "/" {
				ruleParts = append(ruleParts, fmt.Sprintf("PathPrefix(`%s`)", path.Path))
			}
			routerRule := "PathPrefix(`/`)"
			if len(ruleParts) > 0 {
				routerRule = strings.Join(ruleParts, " && ")
			}

			routers.WriteString(fmt.Sprintf("    %s:\n", routerName))
			routers.WriteString(fmt.Sprintf("      rule: \"%s\"\n", routerRule))
			routers.WriteString(fmt.Sprintf("      service: %s\n", routerName))
			if len(ing.Spec.TLS) > 0 {
				routers.WriteString("      tls: {}\n")
			}

			services.WriteString(fmt.Sprintf("    %s:\n", routerName))
			services.WriteString("      loadBalancer:\n")
			services.WriteString("        servers:\n")
			services.WriteString(fmt.Sprintf("          - url: \"http://localhost:%d\"\n", hostPort))
		}
	}

	content := fmt.Sprintf("http:\n  routers:\n%s  services:\n%s", routers.String(), services.String())

	filename := fmt.Sprintf("%s-%s.yaml", ing.Namespace, ing.Name)
	path := filepath.Join(s.config.TraefikDir, filename)
	if err := os.MkdirAll(s.config.TraefikDir, 0755); err != nil {
		fmt.Printf("traefik config: mkdir %s: %v\n", s.config.TraefikDir, err)
		return
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		fmt.Printf("traefik config: write %s: %v\n", path, err)
		return
	}
	fmt.Printf("traefik config: wrote %s\n", filename)
}

// removeTraefikConfig removes the Traefik dynamic config file for an Ingress.
func (s *Server) removeTraefikConfig(ns, name string) {
	if s.config.TraefikDir == "" {
		return
	}
	filename := fmt.Sprintf("%s-%s.yaml", ns, name)
	path := filepath.Join(s.config.TraefikDir, filename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Printf("traefik config: remove %s: %v\n", path, err)
	}
}

// --- Startup reconciliation ---

// ReconcileQuadlets regenerates missing quadlet/timer files for resources in the store.
// Called on startup so containers come back after an uninstall+reinstall.
func (s *Server) ReconcileQuadlets() {
	quadletDir := s.config.QuadletDir
	if quadletDir == "" {
		return
	}

	needReload := false
	var startUnits []string

	missing := func(path string) bool { _, err := os.Stat(path); return err != nil }

	write := func(dir, filename string, content []byte, unitName string) bool {
		if err := writeQuadletFile(dir, filename, content); err != nil {
			fmt.Printf("reconcile: write %s: %v\n", filename, err)
			return false
		}
		fmt.Printf("reconcile: regenerated %s\n", filename)
		needReload = true
		if unitName != "" {
			startUnits = append(startUnits, unitName)
		}
		return true
	}

	for _, dep := range s.config.Store.AllDeployments() {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			instanceName := fmt.Sprintf("%s-%d", dep.Name, i)
			f := fmt.Sprintf("%s/%s-%s.container", quadletDir, dep.Namespace, instanceName)
			if !missing(f) {
				continue
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: dep.Namespace, Labels: dep.Spec.Template.Labels},
				Spec:       dep.Spec.Template.Spec,
			}
			pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
			content, err := quadlet.Container(instanceName, pod, s.config.ConfigDir, s.matchingServiceAliases(dep.Namespace, pod.Labels), s.podPVCMap(dep.Namespace, dep.Spec.Template.Spec))
			if err != nil {
				fmt.Printf("reconcile deployment %s/%s-%d: %v\n", dep.Namespace, dep.Name, i, err)
				continue
			}
			write(quadletDir, fmt.Sprintf("%s-%s.container", dep.Namespace, instanceName), content,
				fmt.Sprintf("%s-%s.service", dep.Namespace, instanceName))
		}
	}

	for _, pod := range s.config.Store.AllPods() {
		f := fmt.Sprintf("%s/%s-%s.container", quadletDir, pod.Namespace, pod.Name)
		if !missing(f) {
			continue
		}
		content, err := quadlet.Container(pod.Name, pod, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, pod.Labels), s.podPVCMap(pod.Namespace, pod.Spec))
		if err != nil {
			fmt.Printf("reconcile pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			continue
		}
		write(quadletDir, fmt.Sprintf("%s-%s.container", pod.Namespace, pod.Name), content,
			fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name))
	}

	for _, pvc := range s.config.Store.AllPVCs() {
		// PVCs bind immediately at creation; this heals claims created
		// before that existed (empty status) and refreshes claims whose
		// volume name predates namespace scoping.
		dirty := false
		if pvc.Status.Phase != corev1.ClaimBound {
			pvc.Status.Phase = corev1.ClaimBound
			dirty = true
		}
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
			class := quadlet.StorageClassStandard
			pvc.Spec.StorageClassName = &class
			dirty = true
		}
		if *pvc.Spec.StorageClassName != quadlet.StorageClassHostPath {
			if want := quadlet.PVCVolumeName(pvc.Namespace, pvc.Name); pvc.Spec.VolumeName != want {
				pvc.Spec.VolumeName = want
				dirty = true
			}
		}
		if dirty {
			if _, err := s.config.Store.UpdatePVC(pvc); err != nil {
				fmt.Printf("reconcile pvc %s/%s: bind: %v\n", pvc.Namespace, pvc.Name, err)
			}
		}
		content, err := quadlet.Volume(pvc)
		if err != nil {
			fmt.Printf("reconcile pvc %s/%s: %v\n", pvc.Namespace, pvc.Name, err)
			continue
		}
		if content == nil {
			// hostpath PVCs don't need a .volume file.
			continue
		}
		// Rewrite only when the file is missing or predates namespace
		// scoping — an existing file with the old unscoped VolumeName would
		// otherwise keep pointing at a volume another deployment may own.
		f := fmt.Sprintf("%s/%s-%s.volume", quadletDir, pvc.Namespace, pvc.Name)
		if existing, _ := os.ReadFile(f); string(existing) == string(content) {
			continue
		}
		write(quadletDir, fmt.Sprintf("%s-%s.volume", pvc.Namespace, pvc.Name), content,
			fmt.Sprintf("%s-%s-volume.service", pvc.Namespace, pvc.Name))
	}

	for _, job := range s.config.Store.AllJobs() {
		f := fmt.Sprintf("%s/%s-%s-job.container", quadletDir, job.Namespace, job.Name)
		if !missing(f) {
			continue
		}
		content, err := quadlet.JobContainer(job.Name, job, s.config.ConfigDir, s.podPVCMap(job.Namespace, job.Spec.Template.Spec))
		if err != nil {
			fmt.Printf("reconcile job %s/%s: %v\n", job.Namespace, job.Name, err)
			continue
		}
		write(quadletDir, fmt.Sprintf("%s-%s-job.container", job.Namespace, job.Name), content,
			fmt.Sprintf("%s-%s-job.service", job.Namespace, job.Name))
	}

	timerDir := s.config.SystemdDir
	if timerDir == "" {
		timerDir = quadletDir
	}
	for _, cj := range s.config.Store.AllCronJobs() {
		cf := fmt.Sprintf("%s/%s-%s-cron.container", quadletDir, cj.Namespace, cj.Name)
		tf := fmt.Sprintf("%s/%s-%s-cron.timer", timerDir, cj.Namespace, cj.Name)
		regen := false
		if missing(cf) {
			content, err := quadlet.CronContainer(cj.Name, cj, s.config.ConfigDir, s.podPVCMap(cj.Namespace, cj.Spec.JobTemplate.Spec.Template.Spec))
			if err != nil {
				fmt.Printf("reconcile cronjob %s/%s container: %v\n", cj.Namespace, cj.Name, err)
				continue
			}
			if write(quadletDir, fmt.Sprintf("%s-%s-cron.container", cj.Namespace, cj.Name), content, "") {
				regen = true
			}
		}
		if missing(tf) {
			content, err := quadlet.CronTimer(cj.Name, cj)
			if err != nil {
				fmt.Printf("reconcile cronjob %s/%s timer: %v\n", cj.Namespace, cj.Name, err)
				continue
			}
			if write(timerDir, fmt.Sprintf("%s-%s-cron.timer", cj.Namespace, cj.Name), content, "") {
				regen = true
			}
		}
		if regen {
			startUnits = append(startUnits, fmt.Sprintf("%s-%s-cron.timer", cj.Namespace, cj.Name))
		}
	}

	mgr := s.config.Manager
	if !needReload || mgr == nil {
		return
	}
	if err := mgr.DaemonReload(); err != nil {
		fmt.Printf("reconcile: daemon-reload failed: %v\n", err)
		return
	}
	for _, unit := range startUnits {
		if err := mgr.StartUnit(unit); err != nil {
			fmt.Printf("reconcile: start %s: %v\n", unit, err)
		}
	}

	// Reconcile Traefik dynamic configs for existing ingresses
	for _, ing := range s.config.Store.AllIngresses() {
		s.generateTraefikConfig(ing)
	}
}

// --- Quadlet integration ---

// podPVCMap builds a map from PVC claim name to PVC object for all PVC
// volumes referenced in the given pod spec. This lets the quadlet generator
// inspect storageClassName and annotations when emitting volume directives.
func (s *Server) podPVCMap(ns string, spec corev1.PodSpec) map[string]*corev1.PersistentVolumeClaim {
	var m map[string]*corev1.PersistentVolumeClaim
	for _, vol := range spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := s.config.Store.GetPVC(ns, vol.PersistentVolumeClaim.ClaimName)
		if err != nil {
			continue
		}
		if m == nil {
			m = make(map[string]*corev1.PersistentVolumeClaim)
		}
		m[pvc.Name] = pvc
	}
	return m
}

func (s *Server) generatePodQuadlet(pod *corev1.Pod) {
	if s.config.QuadletDir == "" {
		return
	}
	resolved := s.resolveEnvFrom(pod)
	content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, resolved.Labels), s.podPVCMap(pod.Namespace, pod.Spec))
	if err != nil {
		fmt.Printf("pod quadlet %s: %v\n", pod.Name, err)
		return
	}
	s.deployUnit(s.config.QuadletDir,
		fmt.Sprintf("%s-%s.container", pod.Namespace, pod.Name),
		content,
		fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name))
}

func (s *Server) redeployPodQuadlet(pod *corev1.Pod) {
	if s.config.QuadletDir == "" {
		return
	}
	resolved := s.resolveEnvFrom(pod)
	content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, resolved.Labels), s.podPVCMap(pod.Namespace, pod.Spec))
	if err != nil {
		fmt.Printf("pod quadlet %s: %v\n", pod.Name, err)
		return
	}
	filename := fmt.Sprintf("%s-%s.container", pod.Namespace, pod.Name)
	unitName := fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name)
	if err := writeQuadletFile(s.config.QuadletDir, filename, content); err != nil {
		fmt.Printf("write %s: %v\n", filename, err)
		return
	}
	mgr := s.config.Manager
	if mgr == nil {
		return
	}
	if err := mgr.DaemonReload(); err != nil {
		fmt.Printf("daemon-reload: %v\n", err)
		return
	}
	mgr.RestartUnit(unitName)
}

func (s *Server) stopPodUnit(pod *corev1.Pod) {
	unit := fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name)
	containerName := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	if mgr := s.config.Manager; mgr != nil {
		mgr.StopUnit(unit)
	}
	// Force-remove synchronously regardless of systemd's involvement.
	// StopUnit only queues an async job — it returns long before a container
	// that ignores SIGTERM actually dies. Without a synchronous, guaranteed
	// removal here, the container stays visible in `podman ps -a` (with its
	// q8s labels intact) after this handler deletes the Store's Pod entry,
	// and the next reconcilePodmanPods tick re-imports it as a zombie pod
	// that — being standalone, not deployment-owned — never gets pruned.
	exec.Command("podman", "rm", "-f", containerName).Run()
}

func (s *Server) removePodQuadlet(pod *corev1.Pod) {
	if s.config.QuadletDir == "" {
		return
	}
	s.reloadAfterRemove(fmt.Sprintf("%s/%s-%s.container", s.config.QuadletDir, pod.Namespace, pod.Name))
}

func (s *Server) generatePVCVolume(pvc *corev1.PersistentVolumeClaim) {
	if s.config.QuadletDir == "" {
		return
	}
	content, err := quadlet.Volume(pvc)
	if err != nil {
		fmt.Printf("pvc volume %s: %v\n", pvc.Name, err)
		s.recordPVCEvent(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "generating quadlet volume: "+err.Error())
		return
	}
	if content == nil {
		// hostpath PVCs don't need a .volume file.
		return
	}
	filename := fmt.Sprintf("%s-%s.volume", pvc.Namespace, pvc.Name)
	unitName := fmt.Sprintf("%s-%s-volume.service", pvc.Namespace, pvc.Name)
	if err := writeQuadletFile(s.config.QuadletDir, filename, content); err != nil {
		fmt.Printf("write pvc volume: %v\n", err)
		s.recordPVCEvent(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "writing quadlet volume: "+err.Error())
		return
	}
	// Immediate provisioning: start the quadlet volume unit so the named
	// podman volume exists right away, not only once a pod happens to mount
	// it. Pods starting later just re-run the idempotent volume create.
	mgr := s.config.Manager
	if mgr == nil {
		return
	}
	if err := mgr.DaemonReload(); err != nil {
		fmt.Printf("daemon-reload: %v\n", err)
		s.recordPVCEvent(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "daemon-reload: "+err.Error())
		return
	}
	// Restart, not start: if the claim name was used before and its volume
	// was removed out-of-band (a Retain-policy volume deleted by hand), the
	// unit may still sit in active/exited state and a plain start would
	// no-op, leaving no volume. The unit is oneshot + idempotent, so
	// re-running it is always safe.
	if err := mgr.RestartUnit(unitName); err != nil {
		fmt.Printf("restart %s: %v\n", unitName, err)
		s.recordPVCEvent(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "starting volume unit: "+err.Error())
	}
}

// recordPVCEvent records a lifecycle event for a PVC.
func (s *Server) recordPVCEvent(pvc *corev1.PersistentVolumeClaim, eventType, reason, message string) {
	s.config.Store.RecordEvent("PersistentVolumeClaim", pvc.Namespace, pvc.Name, pvc.UID, eventType, reason, message)
}

func (s *Server) removePVCVolume(pvc *corev1.PersistentVolumeClaim) {
	if s.config.QuadletDir == "" {
		return
	}
	path := filepath.Join(s.config.QuadletDir, fmt.Sprintf("%s-%s.volume", pvc.Namespace, pvc.Name))
	os.Remove(path)
	if s.config.Manager != nil {
		s.config.Manager.DaemonReload()
	}
}

func (s *Server) generateJobQuadlet(job *batchv1.Job) {
	if s.config.QuadletDir == "" {
		return
	}
	content, err := quadlet.JobContainer(job.Name, job, s.config.ConfigDir, s.podPVCMap(job.Namespace, job.Spec.Template.Spec))
	if err != nil {
		fmt.Printf("job quadlet %s: %v\n", job.Name, err)
		return
	}
	s.deployUnit(s.config.QuadletDir,
		fmt.Sprintf("%s-%s-job.container", job.Namespace, job.Name),
		content,
		fmt.Sprintf("%s-%s-job.service", job.Namespace, job.Name))
}

func (s *Server) stopJobUnit(job *batchv1.Job) {
	if mgr := s.config.Manager; mgr != nil {
		mgr.StopUnit(fmt.Sprintf("%s-%s-job.service", job.Namespace, job.Name))
	}
}

func (s *Server) removeJobQuadlet(job *batchv1.Job) {
	if s.config.QuadletDir == "" {
		return
	}
	s.reloadAfterRemove(fmt.Sprintf("%s/%s-%s-job.container", s.config.QuadletDir, job.Namespace, job.Name))
}

func (s *Server) generateCronJobQuadlets(cj *batchv1.CronJob) {
	quadletDir := s.config.QuadletDir
	if quadletDir == "" {
		return
	}
	timerDir := s.config.SystemdDir
	if timerDir == "" {
		timerDir = quadletDir
	}
	containerContent, err := quadlet.CronContainer(cj.Name, cj, s.config.ConfigDir, s.podPVCMap(cj.Namespace, cj.Spec.JobTemplate.Spec.Template.Spec))
	if err != nil {
		fmt.Printf("cronjob container %s: %v\n", cj.Name, err)
		return
	}
	timerContent, err := quadlet.CronTimer(cj.Name, cj)
	if err != nil {
		fmt.Printf("cronjob timer %s: %v\n", cj.Name, err)
		return
	}
	containerFile := fmt.Sprintf("%s-%s-cron.container", cj.Namespace, cj.Name)
	if err := writeQuadletFile(quadletDir, containerFile, containerContent); err != nil {
		fmt.Printf("write %s: %v\n", containerFile, err)
		return
	}
	timerFile := fmt.Sprintf("%s-%s-cron.timer", cj.Namespace, cj.Name)
	s.deployUnit(timerDir, timerFile, timerContent, timerFile)
}

func (s *Server) removeCronJobQuadlets(cj *batchv1.CronJob) {
	quadletDir := s.config.QuadletDir
	if quadletDir == "" {
		return
	}
	timerDir := s.config.SystemdDir
	if timerDir == "" {
		timerDir = quadletDir
	}
	if mgr := s.config.Manager; mgr != nil {
		mgr.StopUnit(fmt.Sprintf("%s-%s-cron.timer", cj.Namespace, cj.Name))
	}
	s.reloadAfterRemove(
		fmt.Sprintf("%s/%s-%s-cron.container", quadletDir, cj.Namespace, cj.Name),
		fmt.Sprintf("%s/%s-%s-cron.timer", timerDir, cj.Namespace, cj.Name),
	)
}

// jsonMerge applies a JSON merge patch (RFC 7396) onto base in-place.
// Arrays whose elements are objects with a "name" field are merged by name
// rather than replaced, matching strategic-merge-patch semantics for containers/env/volumes.
func jsonMerge(base, overlay map[string]interface{}) {
	for k, v := range overlay {
		if v == nil {
			delete(base, k)
			continue
		}
		if om, ok := v.(map[string]interface{}); ok {
			if bm, ok := base[k].(map[string]interface{}); ok {
				jsonMerge(bm, om)
				continue
			}
		}
		if oa, ok := v.([]interface{}); ok {
			if ba, ok := base[k].([]interface{}); ok {
				if merged := mergeNamedArray(ba, oa); merged != nil {
					base[k] = merged
					continue
				}
			}
		}
		base[k] = v
	}
}

// mergeNamedArray merges two JSON arrays by the "name" key (strategic merge patch semantics).
// Returns nil when elements are not named objects, signalling the caller to use replace semantics.
func mergeNamedArray(base, overlay []interface{}) []interface{} {
	// Require all overlay elements to be objects with a "name" field.
	for _, el := range overlay {
		m, ok := el.(map[string]interface{})
		if !ok {
			return nil
		}
		if _, hasName := m["name"]; !hasName {
			return nil
		}
	}

	// Index base elements by name, preserving order.
	byName := make(map[string]map[string]interface{})
	var order []string
	for _, el := range base {
		m, ok := el.(map[string]interface{})
		if !ok {
			return nil
		}
		name, ok := m["name"].(string)
		if !ok {
			return nil
		}
		byName[name] = m
		order = append(order, name)
	}

	// Merge or append overlay elements.
	for _, el := range overlay {
		m := el.(map[string]interface{})
		name := m["name"].(string)
		if existing, ok := byName[name]; ok {
			jsonMerge(existing, m)
		} else {
			byName[name] = m
			order = append(order, name)
		}
	}

	result := make([]interface{}, 0, len(order))
	seen := make(map[string]bool)
	for _, name := range order {
		if !seen[name] {
			result = append(result, byName[name])
			seen[name] = true
		}
	}
	return result
}

func deploymentReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas < 1 {
		return 1
	}
	return *dep.Spec.Replicas
}

func (s *Server) generateDeploymentQuadlets(dep *appsv1.Deployment) {
	for i := int32(0); i < deploymentReplicas(dep); i++ {
		s.generateDeploymentInstanceQuadlet(dep, i)
	}
}

func (s *Server) generateDeploymentInstanceQuadlet(dep *appsv1.Deployment, i int32) {
	s.deployDeploymentInstance(dep, i, false)
}

func (s *Server) redeployDeploymentInstanceQuadlet(dep *appsv1.Deployment, i int32) {
	s.deployDeploymentInstance(dep, i, true)
}

func (s *Server) deployDeploymentInstance(dep *appsv1.Deployment, i int32, restart bool) {
	if s.config.QuadletDir == "" {
		return
	}
	instanceName := fmt.Sprintf("%s-%d", dep.Name, i)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName,
			Namespace: dep.Namespace,
			Labels:    dep.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       dep.Name,
				UID:        dep.UID,
				Controller: boolPtr(true),
			}},
		},
		Spec: dep.Spec.Template.Spec,
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	resolved := s.resolveEnvFrom(pod)
	content, err := quadlet.Container(instanceName, resolved, s.config.ConfigDir, s.matchingServiceAliases(dep.Namespace, resolved.Labels), s.podPVCMap(dep.Namespace, dep.Spec.Template.Spec))
	if err != nil {
		fmt.Printf("deployment instance quadlet %s/%s-%d: %v\n", dep.Namespace, dep.Name, i, err)
		return
	}
	unitName := fmt.Sprintf("%s-%s.service", dep.Namespace, instanceName)
	if err := writeQuadletFile(s.config.QuadletDir,
		fmt.Sprintf("%s-%s.container", dep.Namespace, instanceName), content); err != nil {
		fmt.Printf("write %s-%s.container: %v\n", dep.Namespace, instanceName, err)
		return
	}
	// Ensure the pod exists in the store immediately so it shows up in
	// `kubectl get pods` even before the container starts (or if it fails).
	if _, err := s.config.Store.GetPod(dep.Namespace, instanceName); err != nil {
		s.config.Store.CreatePod(pod)
	}
	mgr := s.config.Manager
	if mgr == nil {
		return
	}
	if err := mgr.DaemonReload(); err != nil {
		fmt.Printf("daemon-reload: %v\n", err)
		return
	}
	if restart {
		mgr.RestartUnit(unitName)
	} else {
		mgr.StartUnit(unitName)
	}
}

func (s *Server) stopDeploymentUnits(dep *appsv1.Deployment) {
	if mgr := s.config.Manager; mgr != nil {
		for i := int32(0); i < deploymentReplicas(dep); i++ {
			mgr.StopUnit(fmt.Sprintf("%s-%s-%d.service", dep.Namespace, dep.Name, i))
		}
	}
}

func (s *Server) deleteDeploymentPods(dep *appsv1.Deployment) {
	for i := int32(0); i < deploymentReplicas(dep); i++ {
		podName := fmt.Sprintf("%s-%d", dep.Name, i)
		s.config.Store.DeletePod(dep.Namespace, podName)
	}
}

func (s *Server) removeDeploymentQuadlets(dep *appsv1.Deployment) {
	if s.config.QuadletDir == "" {
		return
	}
	n := deploymentReplicas(dep)
	paths := make([]string, n)
	for i := int32(0); i < n; i++ {
		paths[i] = fmt.Sprintf("%s/%s-%s-%d.container", s.config.QuadletDir, dep.Namespace, dep.Name, i)
	}
	s.reloadAfterRemove(paths...)
}

func (s *Server) scaleDeployment(dep *appsv1.Deployment, oldR, newR int32) {
	if s.config.QuadletDir == "" {
		return
	}
	for i := oldR; i < newR; i++ {
		s.generateDeploymentInstanceQuadlet(dep, i)
	}
	for i := newR; i < oldR; i++ {
		instanceName := fmt.Sprintf("%s-%d", dep.Name, i)
		if mgr := s.config.Manager; mgr != nil {
			mgr.StopUnit(fmt.Sprintf("%s-%s.service", dep.Namespace, instanceName))
		}
		s.reloadAfterRemove(fmt.Sprintf("%s/%s-%s.container", s.config.QuadletDir, dep.Namespace, instanceName))
	}
}

func (s *Server) generateNamespaceNetwork(ns string) {
	if s.config.QuadletDir == "" {
		return
	}
	content, err := quadlet.Network(ns)
	if err != nil {
		fmt.Printf("network quadlet %s: %v\n", ns, err)
		return
	}
	s.deployUnit(s.config.QuadletDir, fmt.Sprintf("q8s-%s.network", ns), content, "")
}

func (s *Server) removeNamespaceNetwork(ns string) {
	if s.config.QuadletDir == "" {
		return
	}
	s.reloadAfterRemove(fmt.Sprintf("%s/q8s-%s.network", s.config.QuadletDir, ns))
}

// serviceHostPortConflict checks whether any pod matching the Service's
// selector already has a hostPort that would conflict with one of the
// Service's ports. In q8s the Service socket unit and hostPort both bind
// the same host interface — they're mutually exclusive.
func (s *Server) serviceHostPortConflict(svc *corev1.Service) (int32, bool) {
	for _, pod := range s.config.Store.AllPods() {
		if pod.Namespace != svc.Namespace {
			continue
		}
		if !matchesEqualitySelector(pod.Labels, svc.Spec.Selector) {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.HostPort == 0 {
					continue
				}
				for _, sp := range svc.Spec.Ports {
					if sp.Port == cp.HostPort {
						return cp.HostPort, true
					}
				}
			}
		}
	}
	return 0, false
}

// podHostPortConflict checks whether any Service in the pod's namespace
// already has a socket unit for a port this pod wants as hostPort.
func (s *Server) podHostPortConflict(pod *corev1.Pod) (int32, bool) {
	for _, svc := range s.config.Store.Services(pod.Namespace) {
		if !matchesEqualitySelector(pod.Labels, svc.Spec.Selector) {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.HostPort == 0 {
					continue
				}
				for _, sp := range svc.Spec.Ports {
					if sp.Port == cp.HostPort {
						return cp.HostPort, true
					}
				}
			}
		}
	}
	return 0, false
}

func (s *Server) generateServiceSocket(svc *corev1.Service) {
	quadletDir := s.config.QuadletDir
	if quadletDir == "" {
		return
	}
	for _, port := range svc.Spec.Ports {
		content := fmt.Sprintf(`[Unit]
Description=Socket for service %s/%s port %d

[Socket]
ListenStream=%d

[Install]
WantedBy=sockets.target
`, svc.Namespace, svc.Name, port.Port, port.Port)
		filename := fmt.Sprintf("%s-%d.socket", svc.Name, port.Port)
		if err := writeQuadletFile(quadletDir, filename, []byte(content)); err != nil {
			fmt.Printf("write socket %s: %v\n", filename, err)
		}
	}
}

func (s *Server) writeConfigMapFiles(cm *corev1.ConfigMap) {
	if s.config.ConfigDir == "" {
		return
	}
	dir := filepath.Join(s.config.ConfigDir, cm.Namespace, cm.Name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("configmap dir %s: %v\n", dir, err)
		return
	}
	for k, v := range cm.Data {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0644); err != nil {
			fmt.Printf("write configmap %s/%s/%s: %v\n", cm.Namespace, cm.Name, k, err)
		}
	}
	for k, v := range cm.BinaryData {
		if err := os.WriteFile(filepath.Join(dir, k), v, 0644); err != nil {
			fmt.Printf("write configmap %s/%s/%s: %v\n", cm.Namespace, cm.Name, k, err)
		}
	}
}

func (s *Server) removeConfigMapFiles(cm *corev1.ConfigMap) {
	if s.config.ConfigDir == "" {
		return
	}
	dir := filepath.Join(s.config.ConfigDir, cm.Namespace, cm.Name)
	if err := os.RemoveAll(dir); err != nil {
		fmt.Printf("remove configmap dir %s: %v\n", dir, err)
	}
}

func (s *Server) secretBaseDir() string {
	if s.config.ConfigDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.config.ConfigDir), "secrets")
}

func (s *Server) writeSecretFiles(sec *corev1.Secret) {
	secretDir := s.secretBaseDir()
	if secretDir == "" {
		return
	}
	dir := filepath.Join(secretDir, sec.Namespace, sec.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Printf("secret dir %s: %v\n", dir, err)
		return
	}
	for k, v := range sec.Data {
		if err := os.WriteFile(filepath.Join(dir, k), v, 0600); err != nil {
			fmt.Printf("write secret %s/%s/%s: %v\n", sec.Namespace, sec.Name, k, err)
		}
	}
	for k, v := range sec.StringData {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0600); err != nil {
			fmt.Printf("write secret %s/%s/%s: %v\n", sec.Namespace, sec.Name, k, err)
		}
	}
}

// removeStaleSecretFiles deletes files in the secret's directory for keys
// that no longer exist in the secret. Called after updates (e.g. a JSON
// Patch removing a data key) so pods mounting the directory stop seeing the
// removed key.
func (s *Server) removeStaleSecretFiles(sec *corev1.Secret) {
	secretDir := s.secretBaseDir()
	if secretDir == "" {
		return
	}
	dir := filepath.Join(secretDir, sec.Namespace, sec.Name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	live := make(map[string]bool, len(sec.Data)+len(sec.StringData))
	for k := range sec.Data {
		live[k] = true
	}
	for k := range sec.StringData {
		live[k] = true
	}
	for _, e := range entries {
		if e.IsDir() || live[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			fmt.Printf("remove secret file %s/%s: %v\n", dir, e.Name(), err)
		}
	}
}

func (s *Server) removeSecretFiles(sec *corev1.Secret) {
	secretDir := s.secretBaseDir()
	if secretDir == "" {
		return
	}
	dir := filepath.Join(secretDir, sec.Namespace, sec.Name)
	if err := os.RemoveAll(dir); err != nil {
		fmt.Printf("remove secret dir %s: %v\n", dir, err)
	}
}

// resolveEnvFrom expands envFrom configMapRef/secretRef entries into individual env vars.
// Returns a deep copy of the pod with envFrom replaced by resolved Env entries.
func (s *Server) resolveEnvFrom(pod *corev1.Pod) *corev1.Pod {
	copied := pod.DeepCopy()
	for ci := range copied.Spec.Containers {
		c := &copied.Spec.Containers[ci]
		if len(c.EnvFrom) == 0 {
			continue
		}
		for _, ef := range c.EnvFrom {
			prefix := ef.Prefix
			if ef.ConfigMapRef != nil {
				cm, err := s.config.Store.GetConfigMap(pod.Namespace, ef.ConfigMapRef.Name)
				if err != nil {
					continue
				}
				for k, v := range cm.Data {
					c.Env = append(c.Env, corev1.EnvVar{Name: prefix + k, Value: v})
				}
			}
			if ef.SecretRef != nil {
				sec, err := s.config.Store.GetSecret(pod.Namespace, ef.SecretRef.Name)
				if err != nil {
					continue
				}
				for k, v := range sec.Data {
					c.Env = append(c.Env, corev1.EnvVar{Name: prefix + k, Value: string(v)})
				}
				for k, v := range sec.StringData {
					c.Env = append(c.Env, corev1.EnvVar{Name: prefix + k, Value: v})
				}
			}
		}
		c.EnvFrom = nil
	}
	return copied
}
