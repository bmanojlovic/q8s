package store_test

import (
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"q8s/internal/store"
	"q8s/internal/traefik"
)

func makeMiddleware(ns, name string) *traefik.Middleware {
	return &traefik.Middleware{
		TypeMeta:   metav1.TypeMeta{APIVersion: "traefik.io/v1alpha1", Kind: "Middleware"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": name}},
		Spec: traefik.MiddlewareSpec{
			IPAllowList: &traefik.IPAllowList{
				SourceRange: []string{"10.0.0.0/8"},
				IPStrategy:  &traefik.IPStrategy{Depth: 1, ExcludedIPs: []string{"1.2.3.4"}},
			},
		},
	}
}

func makeIngressRouteTCP(ns, name string) *traefik.IngressRouteTCP {
	return &traefik.IngressRouteTCP{
		TypeMeta:   metav1.TypeMeta{APIVersion: "traefik.io/v1alpha1", Kind: "IngressRouteTCP"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: traefik.IngressRouteTCPSpec{
			EntryPoints: []string{"smtp"},
			Routes: []traefik.RouteTCP{{
				Match: "HostSNI(`*`)",
				Services: []traefik.ServiceTCP{{
					Name:          "stalwart",
					Port:          intstr.FromInt(25),
					ProxyProtocol: &traefik.ProxyProtocol{Version: 2},
				}},
			}},
			TLS: &traefik.TLS{Passthrough: true},
		},
	}
}

func TestTraefikKindsPersistenceRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "store.json")
	st1, err := store.Load(file)
	if err != nil {
		t.Fatalf("Load (new file): %v", err)
	}
	createdMW, err := st1.CreateMiddleware(makeMiddleware("default", "allow"))
	if err != nil {
		t.Fatalf("CreateMiddleware: %v", err)
	}
	createdIRT, err := st1.CreateIngressRouteTCP(makeIngressRouteTCP("default", "smtp"))
	if err != nil {
		t.Fatalf("CreateIngressRouteTCP: %v", err)
	}
	if createdMW.UID == "" || createdIRT.UID == "" {
		t.Fatal("UID not assigned")
	}

	// The store saves asynchronously; reload until both objects are on disk
	// (a fixed sleep was flaky on slower CI runners).
	st2 := reloadUntil(t, file, func(st *store.Store) bool {
		_, errMW := st.GetMiddleware("default", "allow")
		_, errIRT := st.GetIngressRouteTCP("default", "smtp")
		return errMW == nil && errIRT == nil
	})

	mw, err := st2.GetMiddleware("default", "allow")
	if err != nil {
		t.Fatalf("Middleware not restored: %v", err)
	}
	if mw.Spec.IPAllowList == nil || len(mw.Spec.IPAllowList.SourceRange) != 1 ||
		mw.Spec.IPAllowList.IPStrategy == nil || mw.Spec.IPAllowList.IPStrategy.Depth != 1 {
		t.Fatalf("Middleware spec not round-tripped: %+v", mw.Spec)
	}
	if mw.UID != createdMW.UID || mw.ResourceVersion == "" {
		t.Fatalf("Middleware identity not round-tripped: %+v", mw.ObjectMeta)
	}

	irt, err := st2.GetIngressRouteTCP("default", "smtp")
	if err != nil {
		t.Fatalf("IngressRouteTCP not restored: %v", err)
	}
	if len(irt.Spec.EntryPoints) != 1 || irt.Spec.EntryPoints[0] != "smtp" ||
		len(irt.Spec.Routes) != 1 || len(irt.Spec.Routes[0].Services) != 1 {
		t.Fatalf("IngressRouteTCP spec not round-tripped: %+v", irt.Spec)
	}
	if irt.Spec.Routes[0].Services[0].ProxyProtocol == nil || irt.Spec.Routes[0].Services[0].ProxyProtocol.Version != 2 {
		t.Fatalf("proxyProtocol not round-tripped: %+v", irt.Spec.Routes[0].Services[0])
	}
	if irt.Spec.TLS == nil || !irt.Spec.TLS.Passthrough {
		t.Fatalf("tls passthrough not round-tripped: %+v", irt.Spec.TLS)
	}

	// Updates keep the UID and get a new resource version.
	updated := mw.DeepCopy()
	updated.Spec.IPAllowList.SourceRange = []string{"192.168.0.0/16"}
	got, err := st2.UpdateMiddleware(updated)
	if err != nil {
		t.Fatalf("UpdateMiddleware: %v", err)
	}
	if got.UID != mw.UID {
		t.Fatalf("UpdateMiddleware changed UID: %q -> %q", mw.UID, got.UID)
	}
	if got.ResourceVersion == mw.ResourceVersion {
		t.Fatal("UpdateMiddleware did not bump resourceVersion")
	}
}

func TestPurgeNamespaceRemovesTraefikKinds(t *testing.T) {
	st := store.New()
	if _, err := st.CreateMiddleware(makeMiddleware("default", "allow")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateMiddleware(makeMiddleware("other", "allow2")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIngressRouteTCP(makeIngressRouteTCP("default", "smtp")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIngressRouteTCP(makeIngressRouteTCP("other", "imaps")); err != nil {
		t.Fatal(err)
	}

	st.PurgeNamespace("default")

	if _, err := st.GetMiddleware("default", "allow"); err == nil {
		t.Fatal("default Middleware survived namespace purge")
	}
	if _, err := st.GetIngressRouteTCP("default", "smtp"); err == nil {
		t.Fatal("default IngressRouteTCP survived namespace purge")
	}
	if _, err := st.GetMiddleware("other", "allow2"); err != nil {
		t.Fatalf("other namespace Middleware was purged: %v", err)
	}
	if _, err := st.GetIngressRouteTCP("other", "imaps"); err != nil {
		t.Fatalf("other namespace IngressRouteTCP was purged: %v", err)
	}
}
