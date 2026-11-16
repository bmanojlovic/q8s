package server

import (
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"q8s/internal/store"
	"q8s/internal/traefik"
)

// namespaceState holds the objects a namespace delete stopped and unrendered.
// The namespace purge removes them from the store before the namespace entry
// itself is deleted, so a failed final write can only put them back from this
// copy.
type namespaceState struct {
	pods             []*corev1.Pod
	deployments      []*appsv1.Deployment
	jobs             []*batchv1.Job
	cronJobs         []*batchv1.CronJob
	configMaps       []*corev1.ConfigMap
	secrets          []*corev1.Secret
	pvcs             []*corev1.PersistentVolumeClaim
	ingresses        []*networkingv1.Ingress
	middlewares      []*traefik.Middleware
	ingressRouteTCPs []*traefik.IngressRouteTCP
}

// captureNamespaceState copies everything a namespace purge will remove and
// whose files or units a failed namespace delete has to restore.
func (s *Server) captureNamespaceState(ns string) namespaceState {
	return namespaceState{
		pods:             s.config.Store.Pods(ns),
		deployments:      s.config.Store.Deployments(ns),
		jobs:             s.config.Store.Jobs(ns),
		cronJobs:         s.config.Store.CronJobs(ns),
		configMaps:       s.config.Store.ConfigMaps(ns),
		secrets:          s.config.Store.Secrets(ns),
		pvcs:             s.config.Store.PVCs(ns),
		ingresses:        s.config.Store.Ingresses(ns),
		middlewares:      s.config.Store.Middlewares(ns),
		ingressRouteTCPs: s.config.Store.IngressRouteTCPs(ns),
	}
}

// storeDeleteFailed restores a workload whose units were removed before its
// final store delete failed, then returns the store error for the handler's
// respondStoreError mapping. A not-found or conflict is left alone: there is
// no object left to restore.
func (s *Server) storeDeleteFailed(kind, ns, name string, state *namespaceState, err error) error {
	if errors.Is(err, store.ErrPersist) {
		s.restoreAfterFailedDelete(kind, ns, name, state, err)
	}
	return err
}

// restoreAfterFailedDelete is the single restore-on-failed-delete helper. It
// runs the same generators the create paths use, so a workload comes back with
// its quadlet files, a daemon-reload and started units instead of disappearing
// because the final store write failed.
func (s *Server) restoreAfterFailedDelete(kind, ns, name string, state *namespaceState, storeErr error) {
	restoreErr := s.restoreDeletedWorkload(kind, ns, name, state)
	if restoreErr != nil {
		fmt.Printf("delete %s %s/%s: store write failed, workload restored: %v (restore error: %v)\n",
			kind, ns, name, storeErr, restoreErr)
		return
	}
	fmt.Printf("delete %s %s/%s: store write failed, workload restored: %v\n",
		kind, ns, name, storeErr)
}

func (s *Server) restoreDeletedWorkload(kind, ns, name string, state *namespaceState) error {
	switch kind {
	case "pod":
		pod, err := s.config.Store.GetPod(ns, name)
		if err != nil {
			return err
		}
		s.generatePodQuadlet(pod)
		return nil
	case "deployment":
		dep, err := s.config.Store.GetDeployment(ns, name)
		if err != nil {
			return err
		}
		s.generateDeploymentQuadlets(dep)
		return nil
	case "job":
		job, err := s.config.Store.GetJob(ns, name)
		if err != nil {
			return err
		}
		s.generateJobQuadlet(job)
		return nil
	case "cronjob":
		cj, err := s.config.Store.GetCronJob(ns, name)
		if err != nil {
			return err
		}
		s.generateCronJobQuadlets(cj)
		return nil
	case "namespace":
		if state == nil {
			return fmt.Errorf("no captured namespace objects")
		}
		return s.restoreNamespaceState(*state)
	default:
		return fmt.Errorf("no restore path for kind %q", kind)
	}
}

// restoreNamespaceState puts back the files and units of a namespace whose
// purge succeeded but whose namespace entry could not be saved.
func (s *Server) restoreNamespaceState(state namespaceState) error {
	for _, cm := range state.configMaps {
		s.writeConfigMapFiles(cm)
	}
	for _, sec := range state.secrets {
		s.writeSecretFiles(sec)
	}
	for _, pvc := range state.pvcs {
		s.generatePVCVolume(pvc)
	}
	for _, ing := range state.ingresses {
		s.generateTraefikConfig(ing)
	}
	for _, mw := range state.middlewares {
		s.generateMiddlewareConfig(mw)
	}
	for _, irt := range state.ingressRouteTCPs {
		s.generateTCPConfig(irt)
	}
	for _, dep := range state.deployments {
		s.generateDeploymentQuadlets(dep)
	}
	for _, job := range state.jobs {
		s.generateJobQuadlet(job)
	}
	for _, cj := range state.cronJobs {
		s.generateCronJobQuadlets(cj)
	}
	for _, pod := range state.pods {
		s.generatePodQuadlet(pod)
	}
	return nil
}
