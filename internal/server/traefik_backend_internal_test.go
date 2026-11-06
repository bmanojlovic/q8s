package server

import (
	"reflect"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/store"
)

func TestTCPBackendAddressesFollowIngressBackendRules(t *testing.T) {
	st := store.New()
	s := &Server{config: Config{Store: st}}
	if _, err := st.CreateService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mail", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "mail"},
			Ports: []corev1.ServicePort{{
				Name:       "smtp",
				Port:       25,
				TargetPort: intstr.FromInt(2525),
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mail-0", Namespace: "default", Labels: map[string]string{"app": "mail"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "mail", Image: "stalwart",
				Ports: []corev1.ContainerPort{{ContainerPort: 2525}},
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mail-host", Namespace: "default", Labels: map[string]string{"app": "mail"}},
			Spec: corev1.PodSpec{
				HostNetwork: true,
				Containers: []corev1.Container{{
					Name: "mail", Image: "stalwart",
					Ports: []corev1.ContainerPort{{ContainerPort: 2525, HostPort: 20025, HostIP: "127.25.0.1"}},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mail-terminal", Namespace: "default", Labels: map[string]string{"app": "mail"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "mail", Image: "stalwart",
				Ports: []corev1.ContainerPort{{ContainerPort: 2525}},
			}}},
		},
	}
	for _, pod := range pods {
		if _, err := st.CreatePod(pod); err != nil {
			t.Fatal(err)
		}
	}
	st.UpdatePodPhase("default", "mail-terminal", corev1.PodFailed, "")

	got := s.tcpBackendAddresses("default", "mail", 0, "smtp")
	want := []string{"mail-0.default:2525", "127.25.0.1:20025"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tcp backends = %v, want %v", got, want)
	}
}
