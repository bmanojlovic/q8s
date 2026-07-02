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

// respondList handles the table-or-list response pattern common to all list endpoints.
func respondList[Item any, List runtime.Object](
	w http.ResponseWriter,
	r *http.Request,
	rv string,
	items []*Item,
	toTable func([]*Item, string) *table,
	makeList func([]Item) List,
) {
	if isTableRequest(r) {
		encodeTable(w, toTable(items, rv))
		return
	}
	plain := make([]Item, len(items))
	for i, p := range items {
		plain[i] = *p
	}
	encode(w, makeList(plain), http.StatusOK)
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
			respondList(w, r, s.rv(), s.config.Store.Pods(ns), podsToTable,
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
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		created, err := s.config.Store.CreatePod(&pod)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generatePodQuadlet(created)
		created.Status.Phase = corev1.PodPending
		created.Status.StartTime = &metav1.Time{Time: time.Now()}
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

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		if name == "" {
			respondList(w, r, s.rv(), s.config.Store.Services(ns), svcsToTable,
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
		created, err := s.config.Store.CreateService(&svc)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
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
			respondList(w, r, s.rv(), s.config.Store.PVCs(ns), pvcsToTable,
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
		created, err := s.config.Store.CreatePVC(&pvc)
		if err != nil {
			s.respondStatus(w, http.StatusConflict, "AlreadyExists", "%s", err.Error())
			return
		}
		s.generatePVCVolume(created)
		if s.config.Manager != nil {
			s.config.Manager.DaemonReload()
		}
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
			respondList(w, r, s.rv(), s.config.Store.ConfigMaps(ns), configMapsToTable,
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
			respondList(w, r, s.rv(), s.config.Store.Secrets(ns), secretsToTable,
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
		var base, overlay map[string]interface{}
		json.Unmarshal(existing, &base)
		if err := json.Unmarshal(body, &overlay); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		jsonMerge(base, overlay)
		merged, _ := json.Marshal(base)
		var patched corev1.Secret
		if err := json.Unmarshal(merged, &patched); err != nil {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "%s", err.Error())
			return
		}
		updated, err := s.config.Store.UpdateSecret(&patched)
		if err != nil {
			s.respondStatus(w, http.StatusInternalServerError, "InternalError", "%s", err.Error())
			return
		}
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
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "resource %q not found", resource)
	}
}

func (s *Server) handleAppsClusterList(w http.ResponseWriter, r *http.Request) {
	respondList(w, r, s.rv(), s.config.Store.AllDeployments(), deploymentsToTable,
		func(items []appsv1.Deployment) *appsv1.DeploymentList {
			return &appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, Items: items}
		})
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
			respondList(w, r, s.rv(), s.config.Store.Deployments(ns), deploymentsToTable,
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
	rv := s.rv()
	switch resource {
	case "pods":
		respondList(w, r, rv, s.config.Store.AllPods(), podsToTable,
			func(items []corev1.Pod) *corev1.PodList {
				return &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: items}
			})
	case "services":
		respondList(w, r, rv, s.config.Store.AllServices(), svcsToTable,
			func(items []corev1.Service) *corev1.ServiceList {
				return &corev1.ServiceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceList"}, Items: items}
			})
	case "persistentvolumeclaims":
		respondList(w, r, rv, s.config.Store.AllPVCs(), pvcsToTable,
			func(items []corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaimList {
				return &corev1.PersistentVolumeClaimList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"}, Items: items}
			})
	case "configmaps":
		respondList(w, r, rv, s.config.Store.AllConfigMaps(), configMapsToTable,
			func(items []corev1.ConfigMap) *corev1.ConfigMapList {
				return &corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, Items: items}
			})
	case "secrets":
		respondList(w, r, rv, s.config.Store.AllSecrets(), secretsToTable,
			func(items []corev1.Secret) *corev1.SecretList {
				return &corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}, Items: items}
			})
	default:
		s.respondStatus(w, http.StatusNotFound, "NotFound", "%s", fmt.Sprintf("resource %q not found", resource))
	}
}

// --- Namespaces ---

func (s *Server) handleNamespaceList(w http.ResponseWriter, r *http.Request) {
	respondList(w, r, s.rv(), s.config.Store.Namespaces(), namespacesToTable,
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
	rv := s.rv()
	switch resource {
	case "jobs":
		respondList(w, r, rv, s.config.Store.AllJobs(), jobsToTable,
			func(items []batchv1.Job) *batchv1.JobList {
				return &batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}, Items: items}
			})
	case "cronjobs":
		respondList(w, r, rv, s.config.Store.AllCronJobs(), cronJobsToTable,
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
			respondList(w, r, s.rv(), s.config.Store.Jobs(ns), jobsToTable,
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
			if content, err := quadlet.JobContainer(updated.Name, updated, s.config.ConfigDir); err == nil {
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
			respondList(w, r, s.rv(), s.config.Store.CronJobs(ns), cronJobsToTable,
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
				ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: dep.Namespace},
				Spec:       dep.Spec.Template.Spec,
			}
			pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
			content, err := quadlet.Container(instanceName, pod, s.config.ConfigDir)
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
		content, err := quadlet.Container(pod.Name, pod, s.config.ConfigDir)
		if err != nil {
			fmt.Printf("reconcile pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			continue
		}
		write(quadletDir, fmt.Sprintf("%s-%s.container", pod.Namespace, pod.Name), content,
			fmt.Sprintf("%s-%s.service", pod.Namespace, pod.Name))
	}

	for _, pvc := range s.config.Store.AllPVCs() {
		f := fmt.Sprintf("%s/%s-%s.volume", quadletDir, pvc.Namespace, pvc.Name)
		if !missing(f) {
			continue
		}
		content, err := quadlet.Volume(pvc.Name, pvc.Name)
		if err != nil {
			fmt.Printf("reconcile pvc %s/%s: %v\n", pvc.Namespace, pvc.Name, err)
			continue
		}
		write(quadletDir, fmt.Sprintf("%s-%s.volume", pvc.Namespace, pvc.Name), content, "")
	}

	for _, job := range s.config.Store.AllJobs() {
		f := fmt.Sprintf("%s/%s-%s-job.container", quadletDir, job.Namespace, job.Name)
		if !missing(f) {
			continue
		}
		content, err := quadlet.JobContainer(job.Name, job, s.config.ConfigDir)
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
			content, err := quadlet.CronContainer(cj.Name, cj, s.config.ConfigDir)
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
}

// --- Quadlet integration ---

func (s *Server) generatePodQuadlet(pod *corev1.Pod) {
	if s.config.QuadletDir == "" {
		return
	}
	resolved := s.resolveEnvFrom(pod)
	content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir)
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
	content, err := quadlet.Container(pod.Name, resolved, s.config.ConfigDir)
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
		if err := mgr.StopUnit(unit); err != nil {
			// Unit not in systemd (quadlet removed or never loaded) — stop the
			// Podman container directly so it doesn't keep running as an orphan.
			exec.Command("podman", "stop", containerName).Run()
		}
	} else {
		exec.Command("podman", "stop", containerName).Run()
	}
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
	content, err := quadlet.Volume(pvc.Name, pvc.Name)
	if err != nil {
		fmt.Printf("pvc volume %s: %v\n", pvc.Name, err)
		return
	}
	if err := writeQuadletFile(s.config.QuadletDir, fmt.Sprintf("%s-%s.volume", pvc.Namespace, pvc.Name), content); err != nil {
		fmt.Printf("write pvc volume: %v\n", err)
	}
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
	content, err := quadlet.JobContainer(job.Name, job, s.config.ConfigDir)
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
	containerContent, err := quadlet.CronContainer(cj.Name, cj, s.config.ConfigDir)
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
		ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: dep.Namespace},
		Spec:       dep.Spec.Template.Spec,
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	resolved := s.resolveEnvFrom(pod)
	content, err := quadlet.Container(instanceName, resolved, s.config.ConfigDir)
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
