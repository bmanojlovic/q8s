package server

import (
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/quadlet"
	"q8s/internal/store"
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

// readBody reads the request body, mapping MaxBytesReader overflow to a
// 413 Status the way real kube-apiserver does. Returns ok=false (and has
// already written the response) when reading failed.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.respondStatus(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
				"request body exceeds %d bytes", maxRequestBodyBytes)
			return nil, false
		}
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "failed to read request body: %s", err.Error())
		return nil, false
	}
	return body, true
}

// errBodyTooLarge marks a request body rejected by MaxBytesReader so POST
// handlers can answer 413 instead of a generic 400.
var errBodyTooLarge = fmt.Errorf("request body exceeds %d bytes", maxRequestBodyBytes)

// decodeOrRespond decodes a create/update body, writing a 413/400 Status
// and returning false on failure. POST/PUT handlers that would otherwise
// repeat this block use it.
func (s *Server) decodeOrRespond(w http.ResponseWriter, r *http.Request, obj runtime.Object) bool {
	if err := decode(r, obj); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			s.respondStatus(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge", "%s", err.Error())
		} else {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
		}
		return false
	}
	return true
}

func decode(r *http.Request, obj runtime.Object) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
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
		if !s.decodeOrRespond(w, r, &pod) {
			return
		}
		pod.APIVersion = "v1"
		pod.Kind = "Pod"
		if pod.Namespace == "" {
			pod.Namespace = ns
		}
		// k8s defaulting: an unset restartPolicy is Always. Without this,
		// empty reached the quadlet generator, whose defensive Never
		// handling would strand the pod after a clean exit 0.
		if pod.Spec.RestartPolicy == "" {
			pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
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
		if _, err := quadlet.Container(pod.Name, &pod, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, pod.Labels), s.podPVCMap(pod.Namespace, pod.Spec), ""); err != nil {
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
		s.regenerateIngressConfigs(ns)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		pod, err := s.config.Store.GetPod(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(pod)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched corev1.Pod
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if patched.Spec.RestartPolicy == "" {
			patched.Spec.RestartPolicy = corev1.RestartPolicyAlways
		}
		if err := validatePatchedIdentity("pod", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		// Same fail-fast dry-run as create: a patch whose merged spec can't
		// be rendered into a unit file must be rejected here, not stored and
		// then silently left unapplied (200 OK while the running container
		// keeps the old spec).
		if _, err := quadlet.Container(patched.Name, &patched, s.config.ConfigDir, s.matchingServiceAliases(patched.Namespace, patched.Labels), s.podPVCMap(patched.Namespace, patched.Spec), ""); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
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
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, p := range s.config.Store.Pods(ns) {
						names = append(names, p.Name)
					}
					return names
				},
				func(n string) error { return s.deletePodByName(ns, n) })
			return
		}
		if err := s.deletePodByName(ns, name); err != nil {
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
		if !s.decodeOrRespond(w, r, &svc) {
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
		if err := s.reconcileNodePort(created); err != nil {
			s.config.Store.DeleteService(ns, created.Name)
			s.respondStatus(w, http.StatusConflict, "Conflict", "%s", err.Error())
			return
		}
		s.config.Store.UpdateService(created)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		svc, err := s.config.Store.GetService(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(svc)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched corev1.Service
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("service", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
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
		if err := s.reconcileNodePort(updated); err != nil {
			s.respondStatus(w, http.StatusConflict, "Conflict", "%s", err.Error())
			return
		}
		s.config.Store.UpdateService(updated)
		// Clean up legacy per-port socket files (see removeLegacyServiceSockets).
		s.removeLegacyServiceSockets(svc)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, svc := range s.config.Store.Services(ns) {
						names = append(names, svc.Name)
					}
					return names
				},
				func(n string) error { return s.deleteServiceByName(ns, n) })
			return
		}
		if err := s.deleteServiceByName(ns, name); err != nil {
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
		if !s.decodeOrRespond(w, r, &pvc) {
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(pvc)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched corev1.PersistentVolumeClaim
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("persistentvolumeclaim", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdatePVC(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, pvc := range s.config.Store.PVCs(ns) {
						names = append(names, pvc.Name)
					}
					return names
				},
				func(n string) error { return s.deletePVCByName(ns, n) })
			return
		}
		if err := s.deletePVCByName(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
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
		if !s.decodeOrRespond(w, r, &cm) {
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
		if !s.decodeOrRespond(w, r, &cm) {
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(cm)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched corev1.ConfigMap
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("configmap", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
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
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, cm := range s.config.Store.ConfigMaps(ns) {
						names = append(names, cm.Name)
					}
					return names
				},
				func(n string) error { return s.deleteConfigMapByName(ns, n) })
			return
		}
		if err := s.deleteConfigMapByName(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
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
		if !s.decodeOrRespond(w, r, &secret) {
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
		normalizeSecret(&secret)
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
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
		if err := validatePatchedIdentity("secret", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		if err := validateDataKeys(patched.StringData, patched.Data); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		normalizeSecret(&patched)
		updated, err := s.config.Store.UpdateSecret(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		s.removeStaleSecretFiles(updated)
		s.writeSecretFiles(updated)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, sec := range s.config.Store.Secrets(ns) {
						names = append(names, sec.Name)
					}
					return names
				},
				func(n string) error { return s.deleteSecretByName(ns, n) })
			return
		}
		if err := s.deleteSecretByName(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
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
		if !s.decodeOrRespond(w, r, &dep) {
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
		s.regenerateIngressConfigs(ns)
		encode(w, created, http.StatusCreated)
	case http.MethodPatch:
		dep, err := s.config.Store.GetDeployment(ns, name)
		if err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
		oldR := deploymentReplicas(dep)
		// JSON-merge (or RFC 6902 JSON Patch) the patch into the existing
		// deployment so annotations, template changes (e.g. kubectl rollout
		// restart), and replica changes all get persisted correctly.
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(dep)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
			// Terraform's provider sends container/spec changes this way
			// (see the Secret PATCH handler above for the same pattern).
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
		var patched appsv1.Deployment
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("deployment", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
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
		s.regenerateIngressConfigs(ns)
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, dep := range s.config.Store.Deployments(ns) {
						names = append(names, dep.Name)
					}
					return names
				},
				func(n string) error { return s.deleteDeploymentByName(ns, n) })
			return
		}
		if err := s.deleteDeploymentByName(ns, name); err != nil {
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
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
		s.regenerateIngressConfigs(ns)
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
	if !s.decodeOrRespond(w, r, &ns) {
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
	// PVCs: drop the .volume quadlet files. The podman volumes themselves
	// are deliberately kept — data is never deleted automatically.
	for _, pvc := range s.config.Store.PVCs(ns) {
		s.removePVCVolume(pvc)
	}
	// Ingresses: drop the Traefik dynamic configs, or the hostnames keep
	// routing to backends in a namespace that no longer exists.
	for _, ing := range s.config.Store.Ingresses(ns) {
		s.removeTraefikConfig(ing.Namespace, ing.Name)
	}
	// Services: drop legacy per-port socket files (see
	// removeLegacyServiceSockets) along with them.
	for _, svc := range s.config.Store.Services(ns) {
		s.removeLegacyServiceSockets(svc)
	}
	// Secret-derived EnvironmentFiles live in {secretDir}/{ns}/_env/.
	// The per-secret loop above removed the per-secret dirs; removing the
	// namespace parent now also clears _env in one step.
	if secretDir := s.secretBaseDir(); secretDir != "" {
		if err := os.RemoveAll(filepath.Join(secretDir, ns)); err != nil {
			fmt.Printf("remove secret tree %s/%s: %v\n", secretDir, ns, err)
		}
	}
	s.config.Store.PurgeNamespace(ns)
}

// removeLegacyServiceSockets deletes per-port .socket files that very old
// q8s versions wrote for Service ports. That feature never worked (the files
// landed in the quadlet dir, which systemd never loads; nothing bound the
// port) and has been removed — Service ports are a port map for DNS aliases
// and ingress backend resolution, not a host binding. This cleanup exists
// only so upgraded installs don't keep the dead files forever.
func (s *Server) removeLegacyServiceSockets(svc *corev1.Service) {
	if s.config.QuadletDir == "" {
		return
	}
	var paths []string
	for _, port := range svc.Spec.Ports {
		paths = append(paths, filepath.Join(s.config.QuadletDir, fmt.Sprintf("%s-%d.socket", svc.Name, port.Port)))
	}
	if len(paths) > 0 {
		s.reloadAfterRemove(paths...)
	}
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
		if !s.decodeOrRespond(w, r, &job) {
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(job)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched batchv1.Job
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("job", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateJob(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
		if s.config.QuadletDir != "" {
			if resolved, envFile, err := s.resolvedJobEnv(updated); err == nil {
				if content, err := quadlet.JobContainer(resolved.Name, resolved, s.config.ConfigDir, s.podPVCMap(resolved.Namespace, resolved.Spec.Template.Spec), envFile); err == nil {
					writeQuadletFile(s.config.QuadletDir,
						fmt.Sprintf("%s-%s-job.container", resolved.Namespace, resolved.Name), content)
				}
			}
		}
		encode(w, updated, http.StatusOK)
	case http.MethodDelete:
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, j := range s.config.Store.Jobs(ns) {
						names = append(names, j.Name)
					}
					return names
				},
				func(n string) error { return s.deleteJobByName(ns, n) })
			return
		}
		if err := s.deleteJobByName(ns, name); err != nil {
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
		if !s.decodeOrRespond(w, r, &cj) {
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(cj)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched batchv1.CronJob
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("cronjob", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
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
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, cj := range s.config.Store.CronJobs(ns) {
						names = append(names, cj.Name)
					}
					return names
				},
				func(n string) error { return s.deleteCronJobByName(ns, n) })
			return
		}
		if err := s.deleteCronJobByName(ns, name); err != nil {
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
		if !s.decodeOrRespond(w, r, &ing) {
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
		body, ok := s.readBody(w, r)
		if !ok {
			return
		}
		existing, _ := json.Marshal(ing)
		var base map[string]interface{}
		json.Unmarshal(existing, &base)
		if isJSONPatch(r) {
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
		var patched networkingv1.Ingress
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		if err := validatePatchedIdentity("ingress", patched.Namespace, patched.Name, ns, name); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "Invalid", "%s", err.Error())
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
		if name == "" {
			s.deleteCollection(w,
				func() []string {
					var names []string
					for _, ing := range s.config.Store.Ingresses(ns) {
						names = append(names, ing.Name)
					}
					return names
				},
				func(n string) error { return s.deleteIngressByName(ns, n) })
			return
		}
		if err := s.deleteIngressByName(ns, name); err != nil {
			s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", err.Error())
			return
		}
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
			// A path may have no Service backend (resource backends, or an
			// intentionally empty path entry). There is nothing to route to,
			// and dereferencing Backend.Service unconditionally panics —
			// after the object was already stored, leaving the client with
			// a dropped connection and a half-created Ingress.
			if path.Backend.Service == nil {
				continue
			}
			routerName := fmt.Sprintf("%s-%s-%d", ing.Namespace, ing.Name, idx)
			idx++
			svcName := path.Backend.Service.Name
			svcPort := int32(80)
			if path.Backend.Service.Port.Number != 0 {
				svcPort = path.Backend.Service.Port.Number
			}

			// Resolve backend addresses via the Service's selector — the
			// same Endpoints model real k8s uses: every pod the selector
			// matches contributes one server, each with its own port.
			servers := s.ingressBackendServers(ing.Namespace, svcName, svcPort, path.Backend.Service.Port.Name)
			if len(servers) == 0 {
				// No Service (or none with reachable pods): fall back to the
				// ingress-declared port on localhost, preserving the
				// pre-selector behavior.
				servers = []string{fmt.Sprintf("http://localhost:%d", svcPort)}
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
			for _, url := range servers {
				services.WriteString(fmt.Sprintf("          - url: %q\n", url))
			}
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
			pod := s.materializeDeploymentInstance(dep, i)
			// Keep the store pod's ports in sync with the (idempotent)
			// allocation, so ingresses resolve backends for instances whose
			// store objects predate the allocator (or lost their ports).
			if existing, err := s.config.Store.GetPod(dep.Namespace, instanceName); err == nil {
				if len(existing.Spec.Containers) > 0 && len(existing.Spec.Containers[0].Ports) > 0 &&
					existing.Spec.Containers[0].Ports[0].HostPort == 0 &&
					len(pod.Spec.Containers) > 0 && len(pod.Spec.Containers[0].Ports) > 0 {
					existing.Spec.Containers[0].Ports = pod.Spec.Containers[0].Ports
					s.config.Store.UpdatePod(existing)
				}
			}
			resolved, envFile, err := s.resolveEnvFrom(pod)
			if err != nil {
				fmt.Printf("reconcile deployment %s/%s-%d: %v\n", dep.Namespace, dep.Name, i, err)
				continue
			}
			content, err := quadlet.Container(instanceName, resolved, s.config.ConfigDir, s.matchingServiceAliases(dep.Namespace, resolved.Labels), s.podPVCMap(dep.Namespace, dep.Spec.Template.Spec), envFile)
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
		resolved, envFile, err := s.resolveEnvFrom(pod)
		if err != nil {
			fmt.Printf("reconcile pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			continue
		}
		content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, resolved.Labels), s.podPVCMap(pod.Namespace, pod.Spec), envFile)
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
		resolved, envFile, err := s.resolvedJobEnv(job)
		if err != nil {
			fmt.Printf("reconcile job %s/%s: %v\n", job.Namespace, job.Name, err)
			continue
		}
		content, err := quadlet.JobContainer(resolved.Name, resolved, s.config.ConfigDir, s.podPVCMap(resolved.Namespace, resolved.Spec.Template.Spec), envFile)
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
			resolved, envFile, err := s.resolvedCronJobEnv(cj)
			if err != nil {
				fmt.Printf("reconcile cronjob %s/%s container: %v\n", cj.Namespace, cj.Name, err)
				continue
			}
			content, err := quadlet.CronContainer(resolved.Name, resolved, s.config.ConfigDir, s.podPVCMap(resolved.Namespace, resolved.Spec.JobTemplate.Spec.Template.Spec), envFile)
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

// podMaterializeFailed records a Warning event on a pod whose quadlet could
// not be generated/written/deployed, so `kubectl describe pod` shows why the
// running container doesn't match the stored spec — instead of the failure
// living only in the server log.
func (s *Server) podMaterializeFailed(pod *corev1.Pod, stage string, err error) {
	fmt.Printf("pod quadlet %s/%s: %s: %v\n", pod.Namespace, pod.Name, stage, err)
	s.config.Store.RecordEvent("Pod", pod.Namespace, pod.Name, pod.UID,
		corev1.EventTypeWarning, "MaterializationFailed", stage+": "+err.Error())
}

func (s *Server) generatePodQuadlet(pod *corev1.Pod) {
	if s.config.QuadletDir == "" {
		return
	}
	pod = s.applyNodePorts(pod)
	resolved, envFile, err := s.resolveEnvFrom(pod)
	if err != nil {
		s.podMaterializeFailed(pod, "resolving env references", err)
		return
	}
	content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, resolved.Labels), s.podPVCMap(pod.Namespace, pod.Spec), envFile)
	if err != nil {
		s.podMaterializeFailed(pod, "generating quadlet", err)
		return
	}
	s.deployUnit(s.config.QuadletDir,
		fmt.Sprintf("%s-%s.container", pod.Namespace, pod.Name),
		content,
		fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name))
}

// applyNodePorts returns a copy of pod with any NodePort-Service-driven host
// ports bound on 0.0.0.0 (empty HostIP). If no NodePort targets this pod the
// original is returned unchanged. Done at generation time (not stored on the
// pod) so the binding always reflects current Services and never fights the
// hostPort conflict checks on the stored spec.
func (s *Server) applyNodePorts(pod *corev1.Pod) *corev1.Pod {
	bindings := s.nodePortHostPorts(pod)
	if len(bindings) == 0 || len(pod.Spec.Containers) == 0 {
		return pod
	}
	cp := pod.DeepCopy()
	for i := range cp.Spec.Containers[0].Ports {
		p := &cp.Spec.Containers[0].Ports[i]
		if np, ok := bindings[p.ContainerPort]; ok {
			p.HostPort = np
			p.HostIP = "" // all interfaces — real LAN listener
		}
	}
	return cp
}

func (s *Server) redeployPodQuadlet(pod *corev1.Pod) {
	if s.config.QuadletDir == "" {
		return
	}
	pod = s.applyNodePorts(pod)
	resolved, envFile, err := s.resolveEnvFrom(pod)
	if err != nil {
		s.podMaterializeFailed(pod, "resolving env references", err)
		return
	}
	content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir, s.matchingServiceAliases(pod.Namespace, resolved.Labels), s.podPVCMap(pod.Namespace, pod.Spec), envFile)
	if err != nil {
		s.podMaterializeFailed(pod, "generating quadlet", err)
		return
	}
	filename := fmt.Sprintf("%s-%s.container", pod.Namespace, pod.Name)
	unitName := fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name)
	if err := writeQuadletFile(s.config.QuadletDir, filename, content); err != nil {
		s.podMaterializeFailed(pod, "writing "+filename, err)
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
	s.stopAndRemoveUnit(unit, containerName)
}

// stopAndRemoveUnit stops a unit and force-removes its container.
// StopUnit only queues an async job — it returns long before a container
// that ignores SIGTERM actually dies. Without a synchronous, guaranteed
// removal here, the container stays visible in `podman ps -a` (with its
// q8s labels intact) after the store entry is deleted, and the next
// reconcilePodmanPods tick re-imports it. For a standalone pod that means a
// zombie pod that never gets pruned; for a deployment instance it is worse:
// reconcile resurrects the deleted Deployment from a stub spec, and scaled-
// down instances linger as stopped containers forever.
func (s *Server) stopAndRemoveUnit(unit, containerName string) {
	if mgr := s.config.Manager; mgr != nil {
		mgr.StopUnit(unit)
	}
	// Bounded: this runs inside HTTP DELETE handlers, and podman rm -f on a
	// wedged container must not hold the request (and the goroutine) forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "podman", "rm", "-f", containerName).Run()
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
	resolved, envFile, err := s.resolvedJobEnv(job)
	if err != nil {
		fmt.Printf("job quadlet %s: %v\n", job.Name, err)
		return
	}
	content, err := quadlet.JobContainer(resolved.Name, resolved, s.config.ConfigDir, s.podPVCMap(resolved.Namespace, resolved.Spec.Template.Spec), envFile)
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
	resolved, envFile, err := s.resolvedCronJobEnv(cj)
	if err != nil {
		fmt.Printf("cronjob container %s: %v\n", cj.Name, err)
		return
	}
	containerContent, err := quadlet.CronContainer(resolved.Name, resolved, s.config.ConfigDir, s.podPVCMap(resolved.Namespace, resolved.Spec.JobTemplate.Spec.Template.Spec), envFile)
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
	// nil (field omitted) defaults to 1, matching real Kubernetes semantics —
	// but an explicit 0 is a legitimate, meaningful desired state (scaled
	// down) and must NOT be floored to 1, or oldR/newR diffing in
	// scaleDeployment/handleDeploymentScale can never detect a scale-to-zero
	// or a subsequent scale-up from zero (confirmed live 2026-08-29: PATCH
	// {"spec":{"replicas":0}} came back reporting spec.replicas=1).
	if dep.Spec.Replicas == nil {
		return 1
	}
	if *dep.Spec.Replicas < 0 {
		return 0
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

// materializeDeploymentInstance builds the Pod object for deployment
// instance i from the template and injects the allocated per-replica
// hostPorts (loopback-bound) onto the container ports. Deployment replicas
// share one template, so they cannot declare distinct hostPorts themselves —
// the allocator provides them, which is what lets Traefik address each
// replica individually (kube-proxy/Endpoints role) on rootless podman where
// container IPs are unreachable from the host.
func (s *Server) materializeDeploymentInstance(dep *appsv1.Deployment, i int32) *corev1.Pod {
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
		// DeepCopy: a plain struct copy would share the Containers/Ports
		// slices with the template — writing a per-instance hostPort would
		// then mutate the deployment template itself (and every sibling
		// instance would end up with the last-written port).
		Spec: *dep.Spec.Template.Spec.DeepCopy(),
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	if !pod.Spec.HostNetwork && len(pod.Spec.Containers) > 0 {
		for j := range pod.Spec.Containers[0].Ports {
			cp := &pod.Spec.Containers[0].Ports[j]
			if cp.ContainerPort == 0 {
				continue
			}
			cp.HostPort = s.config.Store.AllocateReplicaPort(dep.Namespace, dep.Name, i, cp.ContainerPort)
			cp.HostIP = "127.0.0.1"
		}
	}
	// A NodePort Service targeting this pod publishes its port on 0.0.0.0 as
	// well — the loopback replica port stays (Ingress/Traefik backend), the
	// nodePort adds the real LAN listener. A host port can only be bound by
	// one process, so only instance 0 carries the nodePort publish; higher
	// replicas keep just their loopback port (still Ingress-reachable). This
	// is the single-node NodePort reality — one backing listener per port.
	if i == 0 {
		pod = s.withNodePortPublish(pod)
	}
	return pod
}

// withNodePortPublish appends, for each NodePort Service targeting pod, an
// extra container port entry that publishes containerPort on 0.0.0.0:nodePort
// (empty HostIP = all interfaces). The loopback replica-port entry for the
// same containerPort is left intact, so a pod can be both an Ingress backend
// (loopback) and a NodePort listener (LAN) at once. Returns pod unchanged
// when no NodePort targets it.
func (s *Server) withNodePortPublish(pod *corev1.Pod) *corev1.Pod {
	bindings := s.nodePortHostPorts(pod)
	if len(bindings) == 0 || len(pod.Spec.Containers) == 0 {
		return pod
	}
	for target, np := range bindings {
		pod.Spec.Containers[0].Ports = append(pod.Spec.Containers[0].Ports, corev1.ContainerPort{
			ContainerPort: target,
			HostPort:      np,
			HostIP:        "", // all interfaces
			Protocol:      corev1.ProtocolTCP,
		})
	}
	return pod
}

func (s *Server) deployDeploymentInstance(dep *appsv1.Deployment, i int32, restart bool) {
	if s.config.QuadletDir == "" {
		return
	}
	instanceName := fmt.Sprintf("%s-%d", dep.Name, i)
	pod := s.materializeDeploymentInstance(dep, i)
	resolved, envFile, err := s.resolveEnvFrom(pod)
	if err != nil {
		fmt.Printf("deployment instance quadlet %s/%s-%d: %v\n", dep.Namespace, dep.Name, i, err)
		return
	}
	content, err := quadlet.Container(instanceName, resolved, s.config.ConfigDir, s.matchingServiceAliases(dep.Namespace, resolved.Labels), s.podPVCMap(dep.Namespace, dep.Spec.Template.Spec), envFile)
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
	fmt.Printf("deployment: wrote %s-%s.container\n", dep.Namespace, instanceName)
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
	for i := int32(0); i < deploymentReplicas(dep); i++ {
		s.stopAndRemoveUnit(
			fmt.Sprintf("%s-%s-%d.service", dep.Namespace, dep.Name, i),
			fmt.Sprintf("%s-%s-%d", dep.Namespace, dep.Name, i))
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
		s.stopAndRemoveUnit(
			fmt.Sprintf("%s-%s.service", dep.Namespace, instanceName),
			fmt.Sprintf("%s-%s", dep.Namespace, instanceName))
		s.reloadAfterRemove(fmt.Sprintf("%s/%s-%s.container", s.config.QuadletDir, dep.Namespace, instanceName))
		// Remove the store pod immediately too. The reconcile loop would
		// eventually prune it once the container disappears, but until then
		// ingress backends (selector-driven) would keep routing to a
		// scaled-down replica that no longer exists.
		s.config.Store.DeletePod(dep.Namespace, instanceName)
	}
	// Allocations for removed instances re-enter the pool. Done after the
	// containers are gone so a fresh allocation of the same port (e.g. a
	// quick scale-up) can't race a dying container's publish.
	if oldR > newR {
		s.config.Store.FreeReplicaPorts(dep.Namespace, dep.Name, newR)
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

// assignNodePorts fills in and validates spec.ports[].nodePort for a
// type: NodePort Service. Each port with nodePort==0 gets one allocated from
// the NodePort range; an explicit nodePort must be in range and free. It
// mutates svc in place and returns an error describing the first problem.
//
// A NodePort in q8s is a direct 0.0.0.0 publish on the single backing pod
// (PublishPort=nodePort:targetPort) — dumb L4, protocol-blind, the same
// mechanism as a pod hostPort. Multi-replica load balancing is intentionally
// NOT provided here; use an Ingress (Traefik) for that.
func (s *Server) assignNodePorts(svc *corev1.Service) error {
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		return nil
	}
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if p.NodePort != 0 {
			if !s.config.Store.NodePortAvailable(p.NodePort, key) {
				return fmt.Errorf("nodePort %d is out of range (%d-%d) or already allocated",
					p.NodePort, store.NodePortMin, store.NodePortMax)
			}
			continue
		}
		alloc := s.config.Store.AllocateNodePort(key)
		if alloc == 0 {
			return fmt.Errorf("no free nodePort in range %d-%d", store.NodePortMin, store.NodePortMax)
		}
		p.NodePort = alloc
	}
	return nil
}

// nodePortBackingPods returns the pods a NodePort Service selects. A NodePort
// publishes a single host port on 0.0.0.0, which only one process can bind —
// so the Service must resolve to exactly one backing pod. More than one is
// rejected (use an Ingress for multi-replica exposure).
func (s *Server) nodePortBackingPods(svc *corev1.Service) []*corev1.Pod {
	var pods []*corev1.Pod
	for _, pod := range s.config.Store.Pods(svc.Namespace) {
		if matchesEqualitySelector(pod.Labels, svc.Spec.Selector) {
			pods = append(pods, pod)
		}
	}
	return pods
}

// reconcileNodePort assigns node ports to a type: NodePort Service and
// republishes its single backing pod with those ports bound on 0.0.0.0.
// It is a no-op for non-NodePort Services. Returns an error (surfaced as a
// 409) if node ports can't be assigned or the selector matches more than one
// pod — a single host port can only be bound by one process on one node.
func (s *Server) reconcileNodePort(svc *corev1.Service) error {
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		return nil
	}
	if err := s.assignNodePorts(svc); err != nil {
		return err
	}
	pods := s.nodePortBackingPods(svc)
	if len(pods) > 1 {
		return fmt.Errorf("NodePort service selector matches %d pods; only one backing pod is supported (use an Ingress for multi-replica exposure)", len(pods))
	}
	// No backing pod yet is fine — the port is reserved on the Service and
	// applied when a matching pod appears (see nodePortHostPorts, consulted
	// during pod quadlet generation).
	for _, pod := range pods {
		s.redeployPodQuadlet(pod)
	}
	return nil
}

// nodePortHostPorts returns, for a pod, the set of {containerPort -> nodePort}
// bindings any NodePort Service in the namespace targets at it. The pod's
// quadlet generation publishes these on 0.0.0.0, giving the Service a real
// LAN listener without a proxy.
func (s *Server) nodePortHostPorts(pod *corev1.Pod) map[int32]int32 {
	out := map[int32]int32{}
	for _, svc := range s.config.Store.Services(pod.Namespace) {
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		if !matchesEqualitySelector(pod.Labels, svc.Spec.Selector) {
			continue
		}
		for _, sp := range svc.Spec.Ports {
			if sp.NodePort == 0 {
				continue
			}
			target := sp.TargetPort.IntVal
			if target == 0 {
				target = sp.Port
			}
			out[target] = sp.NodePort
		}
	}
	return out
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

// normalizeSecret folds StringData into Data the way the Kubernetes API
// server does: each StringData entry is base64-less raw text that becomes a
// Data ([]byte) entry, StringData wins on key conflicts, and StringData is
// then cleared so Data is the single source of truth. Without this, a Secret
// created with only stringData (e.g. Terraform's tls_self_signed_cert PEMs)
// keeps its real content solely in StringData — which the store persists but
// restoreSecretFiles (Data-only) can't rewrite after a restart, silently
// emptying the on-disk files. See tic-b31a.
func normalizeSecret(sec *corev1.Secret) {
	if len(sec.StringData) == 0 {
		return
	}
	if sec.Data == nil {
		sec.Data = make(map[string][]byte, len(sec.StringData))
	}
	for k, v := range sec.StringData {
		sec.Data[k] = []byte(v)
	}
	sec.StringData = nil
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
	// Data is the single source of truth — StringData is folded into it by
	// normalizeSecret before the object is ever stored.
	for k, v := range sec.Data {
		if err := os.WriteFile(filepath.Join(dir, k), v, 0600); err != nil {
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

// resolveEnvFrom resolves a Pod's env references and returns a deep copy plus
// the path to a 0600 EnvironmentFile holding any Secret-derived values (empty
// string if none). Fails if a non-optional ConfigMap/Secret/key ref can't be
// resolved -- matching real Kubernetes, which refuses to start such a pod
// (CreateContainerConfigError) rather than silently running it with a blank
// value.
func (s *Server) resolveEnvFrom(pod *corev1.Pod) (*corev1.Pod, string, error) {
	copied := pod.DeepCopy()
	secretNames, err := s.resolveContainerEnv(pod.Namespace, copied.Spec.Containers)
	if err != nil {
		return nil, "", err
	}
	envFile, err := s.writeEnvFile(pod.Namespace, pod.Name, copied.Spec.Containers, secretNames)
	if err != nil {
		return nil, "", err
	}
	return copied, envFile, nil
}

// resolvedJobEnv is resolveEnvFrom for a Job's pod template.
func (s *Server) resolvedJobEnv(job *batchv1.Job) (*batchv1.Job, string, error) {
	resolved := job.DeepCopy()
	secretNames, err := s.resolveContainerEnv(resolved.Namespace, resolved.Spec.Template.Spec.Containers)
	if err != nil {
		return nil, "", err
	}
	envFile, err := s.writeEnvFile(resolved.Namespace, resolved.Name+"-job", resolved.Spec.Template.Spec.Containers, secretNames)
	if err != nil {
		return nil, "", err
	}
	return resolved, envFile, nil
}

// resolvedCronJobEnv is resolveEnvFrom for a CronJob's pod template.
func (s *Server) resolvedCronJobEnv(cj *batchv1.CronJob) (*batchv1.CronJob, string, error) {
	resolved := cj.DeepCopy()
	secretNames, err := s.resolveContainerEnv(resolved.Namespace, resolved.Spec.JobTemplate.Spec.Template.Spec.Containers)
	if err != nil {
		return nil, "", err
	}
	envFile, err := s.writeEnvFile(resolved.Namespace, resolved.Name+"-cron", resolved.Spec.JobTemplate.Spec.Template.Spec.Containers, secretNames)
	if err != nil {
		return nil, "", err
	}
	return resolved, envFile, nil
}

// resolveContainerEnv resolves Env[i].ValueFrom.{ConfigMapKeyRef,SecretKeyRef}
// and EnvFrom references in place. The quadlet generator only ever emits
// env.Value verbatim, so an unresolved ValueFrom silently renders as an
// empty Environment= line (confirmed live 2026-08-29: MOZAK_VIEW_TOKEN etc.
// all came out blank, and the app exited 0 on missing config instead of
// crash-looping visibly). Returns the set of env var names whose value came
// from a Secret, so callers can route them to a 0600 EnvironmentFile instead
// of the world-readable-ish quadlet .container file, and errors out if a
// non-optional ref can't be resolved instead of leaving it silently blank.
func (s *Server) resolveContainerEnv(ns string, containers []corev1.Container) (map[string]bool, error) {
	secretNames := map[string]bool{}
	for ci := range containers {
		c := &containers[ci]
		for ei := range c.Env {
			ev := &c.Env[ei]
			if ev.ValueFrom == nil {
				continue
			}
			if ref := ev.ValueFrom.ConfigMapKeyRef; ref != nil {
				cm, err := s.config.Store.GetConfigMap(ns, ref.Name)
				v, ok := "", false
				if err == nil {
					v, ok = cm.Data[ref.Key]
				}
				if !ok {
					if ref.Optional != nil && *ref.Optional {
						continue
					}
					return nil, fmt.Errorf("env %s: configMapKeyRef %s/%s key %q not found", ev.Name, ns, ref.Name, ref.Key)
				}
				ev.Value = v
			}
			if ref := ev.ValueFrom.SecretKeyRef; ref != nil {
				sec, err := s.config.Store.GetSecret(ns, ref.Name)
				v, ok := "", false
				if err == nil {
					if bv, bok := sec.Data[ref.Key]; bok {
						v, ok = string(bv), true
					} else if sv, sok := sec.StringData[ref.Key]; sok {
						v, ok = sv, true
					}
				}
				if !ok {
					if ref.Optional != nil && *ref.Optional {
						continue
					}
					return nil, fmt.Errorf("env %s: secretKeyRef %s/%s key %q not found", ev.Name, ns, ref.Name, ref.Key)
				}
				ev.Value = v
				secretNames[ev.Name] = true
			}
			ev.ValueFrom = nil
		}
		if len(c.EnvFrom) == 0 {
			continue
		}
		for _, ef := range c.EnvFrom {
			prefix := ef.Prefix
			if ef.ConfigMapRef != nil {
				cm, err := s.config.Store.GetConfigMap(ns, ef.ConfigMapRef.Name)
				if err != nil {
					if ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional {
						continue
					}
					return nil, fmt.Errorf("envFrom: configMapRef %s/%s not found", ns, ef.ConfigMapRef.Name)
				}
				for k, v := range cm.Data {
					c.Env = append(c.Env, corev1.EnvVar{Name: prefix + k, Value: v})
				}
			}
			if ef.SecretRef != nil {
				sec, err := s.config.Store.GetSecret(ns, ef.SecretRef.Name)
				if err != nil {
					if ef.SecretRef.Optional != nil && *ef.SecretRef.Optional {
						continue
					}
					return nil, fmt.Errorf("envFrom: secretRef %s/%s not found", ns, ef.SecretRef.Name)
				}
				for k, v := range sec.Data {
					c.Env = append(c.Env, corev1.EnvVar{Name: prefix + k, Value: string(v)})
					secretNames[prefix+k] = true
				}
				for k, v := range sec.StringData {
					c.Env = append(c.Env, corev1.EnvVar{Name: prefix + k, Value: v})
					secretNames[prefix+k] = true
				}
			}
		}
		c.EnvFrom = nil
	}
	return secretNames, nil
}

// writeEnvFile splits any Secret-derived Env entries (per secretNames) out of
// containers[0].Env and writes them as a 0600 systemd EnvironmentFile, next to
// the per-secret files writeSecretFiles already writes at the same
// permissions -- unlike those, they don't otherwise land on disk anywhere, so
// without this they'd only ever have been emitted as Environment= lines in
// the quadlet .container file, which writeQuadletFile writes 0644 (confirmed
// live 2026-08-29: MOZAK_VIEW_TOKEN and DEEPSEEK_API_KEY were both readable
// in that file to any local user). Returns "" if there was nothing to split.
func (s *Server) writeEnvFile(ns, name string, containers []corev1.Container, secretNames map[string]bool) (string, error) {
	if len(containers) == 0 || len(secretNames) == 0 {
		return "", nil
	}
	c := &containers[0]
	var lines strings.Builder
	kept := c.Env[:0]
	for _, ev := range c.Env {
		if !secretNames[ev.Name] {
			kept = append(kept, ev)
			continue
		}
		if strings.ContainsAny(ev.Value, "\n\r\x00") {
			return "", fmt.Errorf("env %s: secret value must not contain control characters", ev.Name)
		}
		lines.WriteString(ev.Name)
		lines.WriteByte('=')
		lines.WriteString(ev.Value)
		lines.WriteByte('\n')
	}
	c.Env = kept
	if lines.Len() == 0 {
		return "", nil
	}
	secretDir := s.secretBaseDir()
	if secretDir == "" {
		return "", fmt.Errorf("no secret directory configured, cannot write env file for %s/%s", ns, name)
	}
	dir := filepath.Join(secretDir, ns, "_env")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("env dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+".env")
	if err := os.WriteFile(path, []byte(lines.String()), 0600); err != nil {
		return "", fmt.Errorf("write env file %s: %w", path, err)
	}
	return path, nil
}

// ingressBackendServers resolves the backend URLs for one ingress path: the
// Service's selector -> matching pods -> one URL per pod, in the spirit of
// k8s Endpoints. Each pod contributes its own published hostPort, so pods
// behind one Service do not need identical ports — only matching labels and
// a container port equal to the Service's targetPort.
//
// Pods without a hostPort are skipped: on rootless podman, container
// addresses are only reachable from the host through published ports, so a
// pod that publishes nothing is not addressable by an on-host Traefik. (An
// allocated-port scheme for deployment replicas — where the shared template
// can't declare distinct hostPorts — is a follow-up; today this serves
// standalone pods and single-replica deployments.)
//
// Returns nil when there is no such Service or no pod yields an address;
// callers fall back to the ingress-declared port.
func (s *Server) ingressBackendServers(ns, svcName string, svcPort int32, portName string) []string {
	svc, err := s.config.Store.GetService(ns, svcName)
	if err != nil {
		return nil
	}

	// Which ServicePort (and thus targetPort) the ingress refers to.
	// targetPort may be numeric or a container-port name; resolve both.
	targetNum := svcPort
	targetName := ""
	for _, p := range svc.Spec.Ports {
		if (svcPort != 0 && p.Port == svcPort) || (portName != "" && p.Name == portName) {
			if p.TargetPort.Type == intstr.String {
				targetName = p.TargetPort.StrVal
			} else if v := p.TargetPort.IntValue(); v != 0 {
				targetNum = int32(v)
			}
			break
		}
	}

	var servers []string
	for _, pod := range s.config.Store.Pods(ns) {
		if !matchesEqualitySelector(pod.Labels, svc.Spec.Selector) {
			continue
		}
		// Endpoints-like liveness: terminal pods no longer have a listener
		// behind their port, so they leave the rotation (their published
		// port only binds while the container runs anyway).
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for _, cp := range c.Ports {
				if cp.ContainerPort != targetNum && cp.Name != targetName {
					continue
				}
				if cp.HostPort != 0 {
					servers = append(servers, fmt.Sprintf("http://127.0.0.1:%d", cp.HostPort))
				}
				// A pod lists each container port once; stop at the first
				// matching entry for this container.
				break
			}
		}
	}
	return servers
}

// regenerateIngressConfigs rewrites the Traefik dynamic config for every
// ingress in a namespace. Called whenever pod churn can change the backend
// set a selector resolves to (pod create/delete, deployment scale/redeploy)
// so the servers list tracks reality the way Endpoints do.
func (s *Server) regenerateIngressConfigs(ns string) {
	if s.config.TraefikDir == "" {
		return
	}
	for _, ing := range s.config.Store.Ingresses(ns) {
		s.generateTraefikConfig(ing)
	}
}

// --- collection delete (deletecollection) ---
//
// Discovery advertises deletecollection for every resource, and kubectl
// uses it for `kubectl delete pods --all`. These helpers give each DELETE
// handler a single-object cleanup routine reused by the collection path.

// deleteCollection deletes every name listFn returns via delFn and answers
// with one Success Status. Errors on individual objects are logged and
// skipped — a collection delete is best-effort like real k8s.
func (s *Server) deleteCollection(w http.ResponseWriter, listFn func() []string, delFn func(name string) error) {
	deleted := 0
	for _, n := range listFn() {
		if err := delFn(n); err != nil {
			fmt.Printf("deletecollection %s: %v\n", n, err)
			continue
		}
		deleted++
	}
	encode(w, &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("deleted %d object(s)", deleted),
	}, http.StatusOK)
}

func (s *Server) deletePodByName(ns, name string) error {
	pod, err := s.config.Store.GetPod(ns, name)
	if err != nil {
		return err
	}
	s.stopPodUnit(pod)
	s.removePodQuadlet(pod)
	if err := s.config.Store.DeletePod(ns, name); err != nil {
		return err
	}
	s.regenerateIngressConfigs(ns)
	return nil
}

func (s *Server) deleteServiceByName(ns, name string) error {
	svc, _ := s.config.Store.GetService(ns, name)
	if err := s.config.Store.DeleteService(ns, name); err != nil {
		return err
	}
	if svc != nil {
		s.removeLegacyServiceSockets(svc)
	}
	return nil
}

func (s *Server) deletePVCByName(ns, name string) error {
	pvc, _ := s.config.Store.GetPVC(ns, name)
	if err := s.config.Store.DeletePVC(ns, name); err != nil {
		return err
	}
	if pvc != nil {
		s.removePVCVolume(pvc)
	}
	return nil
}

func (s *Server) deleteConfigMapByName(ns, name string) error {
	cm, _ := s.config.Store.GetConfigMap(ns, name)
	if err := s.config.Store.DeleteConfigMap(ns, name); err != nil {
		return err
	}
	if cm != nil {
		s.removeConfigMapFiles(cm)
	}
	return nil
}

func (s *Server) deleteSecretByName(ns, name string) error {
	sec, _ := s.config.Store.GetSecret(ns, name)
	if err := s.config.Store.DeleteSecret(ns, name); err != nil {
		return err
	}
	if sec != nil {
		s.removeSecretFiles(sec)
	}
	return nil
}

func (s *Server) deleteDeploymentByName(ns, name string) error {
	dep, err := s.config.Store.GetDeployment(ns, name)
	if err != nil {
		return err
	}
	s.stopDeploymentUnits(dep)
	s.removeDeploymentQuadlets(dep)
	s.deleteDeploymentPods(dep)
	if err := s.config.Store.DeleteDeployment(ns, name); err != nil {
		return err
	}
	s.config.Store.FreeReplicaPorts(dep.Namespace, dep.Name, 0)
	s.regenerateIngressConfigs(dep.Namespace)
	return nil
}

func (s *Server) deleteJobByName(ns, name string) error {
	job, err := s.config.Store.GetJob(ns, name)
	if err != nil {
		return err
	}
	s.stopJobUnit(job)
	s.removeJobQuadlet(job)
	return s.config.Store.DeleteJob(ns, name)
}

func (s *Server) deleteCronJobByName(ns, name string) error {
	cj, err := s.config.Store.GetCronJob(ns, name)
	if err != nil {
		return err
	}
	s.removeCronJobQuadlets(cj)
	return s.config.Store.DeleteCronJob(ns, name)
}

func (s *Server) deleteIngressByName(ns, name string) error {
	if err := s.config.Store.DeleteIngress(ns, name); err != nil {
		return err
	}
	s.removeTraefikConfig(ns, name)
	return nil
}
