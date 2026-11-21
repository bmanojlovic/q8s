package names_test

import (
	"regexp"
	"strings"
	"testing"

	"q8s/internal/names"
)

// kindCase pins one generated artifact per supported kind. The legacy column
// is the current on-host format and the new column is the format PR 2 will
// switch to.
type kindCase struct {
	kind       names.Kind
	namespace  string
	name       string
	extra      []string
	wantNew    string
	wantLegacy string
}

func kindCases() []kindCase {
	return []kindCase{
		{names.KindPod, "team", "prod-db", nil, "team_prod-db", "team-prod-db"},
		{names.KindPodContainer, "team", "a", []string{"b-c"}, "team_a_b-c", "team-a-b-c"},
		{names.KindPodContainer, "team", "a", []string{""}, "team_a", "team-a"},
		{names.KindPodFile, "team", "prod-db", nil, "team_prod-db.pod", "team-prod-db.pod"},
		{names.KindPodService, "team", "prod-db", nil, "team_prod-db.service", "team-prod-db.service"},
		{names.KindPodUnit, "team", "prod-db", nil, "team_prod-db-pod.service", "team-prod-db-pod.service"},
		{names.KindPodContainerFile, "team", "a", []string{"b-c"}, "team_a_b-c.container", "team-a-b-c.container"},
		{names.KindPodContainerFile, "team", "a", nil, "team_a.container", "team-a.container"},
		{names.KindPodContainerService, "team", "a", []string{"b-c"}, "team_a_b-c.service", "team-a-b-c.service"},
		{names.KindPodContainerService, "team", "a", nil, "team_a.service", "team-a.service"},
		{names.KindJob, "team", "x", nil, "team_x__job", "team-x-job"},
		{names.KindJobFile, "team", "x", nil, "team_x__job.container", "team-x-job.container"},
		{names.KindJobService, "team", "x", nil, "team_x__job.service", "team-x-job.service"},
		{names.KindCronJob, "team", "x", nil, "team_x__cron", "team-x-cron"},
		{names.KindCronJobFile, "team", "x", nil, "team_x__cron.container", "team-x-cron.container"},
		{names.KindCronJobService, "team", "x", nil, "team_x__cron.service", "team-x-cron.service"},
		{names.KindCronJobTimer, "team", "x", nil, "team_x__cron.timer", "team-x-cron.timer"},
		{names.KindEnvPod, "team", "prod-db", nil, "prod-db.env", "prod-db.env"},
		{names.KindEnvJob, "team", "x", nil, "x__job.env", "x-job.env"},
		{names.KindEnvCronJob, "team", "x", nil, "x__cron.env", "x-cron.env"},
		{names.KindPVCVolume, "team", "data", nil, "team_data", "team-data"},
		{names.KindPVCFile, "team", "data", nil, "team_data.volume", "team-data.volume"},
		{names.KindDeploymentPod, "", "web", []string{"1"}, "web-1", "web-1"},
		{names.KindDeploymentHost, "team", "web", []string{"1"}, "team_web-1", "team-web-1"},
		{names.KindDeploymentVolumeOwner, "", "web", []string{"1"}, "web_1", "web_1"},
		{names.KindSocketFile, "team", "app", []string{"http"}, "team_app_http.socket", "team-app-http.socket"},
		{names.KindSocketService, "team", "app", nil, "team_app.service", "team-app.service"},
		{names.KindIngressFile, "team", "web", nil, "team_web.yaml", "team-web.yaml"},
		{names.KindMiddlewareFile, "team", "web", nil, "middleware_team_web.yaml", "middleware-team-web.yaml"},
		{names.KindIngressRouteTCPFile, "team", "web", nil, "tcp_team_web.yaml", "tcp-team-web.yaml"},
		{names.KindIngressRoute, "team", "web", []string{"0"}, "team_web_0", "team-web-0"},
		{names.KindIngressRouteTCP, "team", "web", []string{"0"}, "team_web_0", "team-web-0"},
		{names.KindMiddlewareKey, "team", "web", nil, "team-web", "team-web"},
		{names.KindNetwork, "team", "", nil, "q8s-team", "q8s-team"},
		{names.KindEmptyDirVolume, "team", "web", []string{"data"}, "q8s_team_web_data", "q8s_team_web_data"},
	}
}

func TestHostAndLegacyHostPerKind(t *testing.T) {
	for _, tc := range kindCases() {
		tc := tc
		t.Run(string(tc.kind)+"/"+tc.namespace+"/"+tc.name, func(t *testing.T) {
			if got := names.Host(tc.kind, tc.namespace, tc.name, tc.extra...); got != tc.wantNew {
				t.Errorf("Host(%s, %q, %q, %v) = %q, want %q", tc.kind, tc.namespace, tc.name, tc.extra, got, tc.wantNew)
			}
			if got := names.LegacyHost(tc.kind, tc.namespace, tc.name, tc.extra...); got != tc.wantLegacy {
				t.Errorf("LegacyHost(%s, %q, %q, %v) = %q, want %q", tc.kind, tc.namespace, tc.name, tc.extra, got, tc.wantLegacy)
			}
		})
	}
}

// TestNewNamesUseAcceptedCharacters checks the public character contract from
// section 2.1: every consumer accepts letters, digits, "_", "." and "-", so
// the generated names stay inside that intersection.
func TestNewNamesUseAcceptedCharacters(t *testing.T) {
	accepted := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	for _, tc := range kindCases() {
		got := names.Host(tc.kind, tc.namespace, tc.name, tc.extra...)
		if !accepted.MatchString(got) {
			t.Errorf("Host(%s, %q, %q, %v) = %q contains a character no consumer accepts", tc.kind, tc.namespace, tc.name, tc.extra, got)
		}
	}
}

// TestNamespaceNameSplitsDiffer checks both sides of the split that used to
// collide.
func TestNamespaceNameSplitsDiffer(t *testing.T) {
	a := names.Host(names.KindPod, "team", "prod-db")
	b := names.Host(names.KindPod, "team-prod", "db")
	if a == b {
		t.Fatalf("team/prod-db and team-prod/db both became %q", a)
	}
	if names.LegacyHost(names.KindPod, "team", "prod-db") != names.LegacyHost(names.KindPod, "team-prod", "db") {
		t.Fatalf("the legacy format no longer demonstrates the ambiguity")
	}
}

func TestJobAndPodNamesDiffer(t *testing.T) {
	if a, b := names.Host(names.KindJob, "team", "x"), names.Host(names.KindPod, "team", "x-job"); a == b {
		t.Errorf("Job x and Pod x-job both became %q", a)
	}
	if a, b := names.Host(names.KindEnvJob, "team", "x"), names.Host(names.KindEnvPod, "team", "x-job"); a == b {
		t.Errorf("Job x and Pod x-job env files both became %q", a)
	}
	if a, b := names.Host(names.KindCronJob, "team", "x"), names.Host(names.KindPod, "team", "x-cron"); a == b {
		t.Errorf("CronJob x and Pod x-cron both became %q", a)
	}
	if a, b := names.Host(names.KindEnvCronJob, "team", "x"), names.Host(names.KindEnvPod, "team", "x-cron"); a == b {
		t.Errorf("CronJob x and Pod x-cron env files both became %q", a)
	}
}

// TestKindSuffixDiffersFromMemberName covers a shape a single "_" suffix
// would not separate: a Job x and a multi-container Pod x with a member named
// "job" (likewise "cron"). The kind suffix uses "__", which no Kubernetes name
// can contain, so the two stay distinct.
func TestKindSuffixDiffersFromMemberName(t *testing.T) {
	if a, b := names.Host(names.KindJob, "team", "x"), names.Host(names.KindPodContainer, "team", "x", "job"); a == b {
		t.Errorf("Job x and Pod x/container job both became %q", a)
	}
	if a, b := names.Host(names.KindCronJob, "team", "x"), names.Host(names.KindPodContainer, "team", "x", "cron"); a == b {
		t.Errorf("CronJob x and Pod x/container cron both became %q", a)
	}
}

// TestCompactedNameDiffersFromMemberName: a compacted name ends in a hash a
// client can compute, and a container may be named with 16 hex characters. The
// hash is separated by "__" so a short Pod with such a container cannot take
// the compacted name of a long Pod.
func TestCompactedNameDiffersFromMemberName(t *testing.T) {
	long := strings.Repeat("p", 253)
	compacted := names.Host(names.KindPod, "team", long)
	i := strings.LastIndex(compacted, "__")
	if i < 0 {
		t.Fatalf("compacted name %q has no __ hash separator", compacted)
	}
	head, hash := compacted[:i], compacted[i+2:]
	j := strings.Index(head, "_")
	if j < 0 {
		t.Fatalf("compacted name %q has no namespace separator", compacted)
	}
	member := names.Host(names.KindPodContainer, "team", head[j+1:], hash)
	if member == compacted {
		t.Fatalf("Pod %s/container %s took the compacted name %q", head[j+1:], hash, compacted)
	}
}

func TestMultiContainerMemberNamesDiffer(t *testing.T) {
	a := names.Host(names.KindPodContainer, "team", "a", "b-c")
	b := names.Host(names.KindPodContainer, "team", "a-b", "c")
	if a == b {
		t.Fatalf("Pod a/container b-c and Pod a-b/container c both became %q", a)
	}
}

// TestDeploymentInstanceNames pins the design's deliberate split: the API Pod
// name is unchanged, while the host prefix uses the new separator. A Deployment
// web replica 1 and a Pod named web-1 are the same API object name, so they
// must produce the same host name rather than two different ones.
func TestDeploymentInstanceNames(t *testing.T) {
	api := names.Host(names.KindDeploymentPod, "", "web", "1")
	if api != "web-1" {
		t.Fatalf("Deployment API Pod name = %q, want web-1", api)
	}
	host := names.Host(names.KindDeploymentHost, "team", "web", "1")
	pod := names.Host(names.KindPod, "team", "web-1")
	if host != "team_web-1" {
		t.Fatalf("Deployment host prefix = %q, want team_web-1", host)
	}
	if host != pod {
		t.Fatalf("Deployment web replica 1 host %q differs from Pod web-1 host %q, but they are the same API name", host, pod)
	}
	if host == names.Host(names.KindPod, "team", "web") {
		t.Fatalf("Deployment web replica 1 collides with Pod web")
	}
	if host == names.Host(names.KindDeploymentHost, "team", "web", "10") {
		t.Fatalf("Deployment replica 1 collides with replica 10")
	}

	longDeployment := strings.Repeat("d", 253)
	longHost := names.Host(names.KindDeploymentHost, "team", longDeployment, "1")
	longPod := names.Host(names.KindPod, "team", longDeployment+"-1")
	if longHost != longPod {
		t.Fatalf("long Deployment instance host %q differs from Pod host %q", longHost, longPod)
	}
}

func TestCompactBoundaryAndFallback(t *testing.T) {
	first := strings.Repeat("a", 100)
	tests := []struct {
		name      string
		namespace string
		object    string
		wantHash  bool
	}{
		{"short input", "team", "prod-db", false},
		{"exactly at the cap", first, strings.Repeat("b", 139), false},
		{"one byte over the cap", first, strings.Repeat("b", 140), true},
		{"253 byte namespace", strings.Repeat("a", 253), "b", true},
		{"253 byte object name", "a", strings.Repeat("b", 253), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := names.Host(names.KindPod, tc.namespace, tc.object)
			if len(got) > names.MaxBaseLen {
				t.Fatalf("name length = %d, want <= %d", len(got), names.MaxBaseLen)
			}
			hasHash := regexp.MustCompile(`_[0-9a-f]{16}$`).MatchString(got)
			if hasHash != tc.wantHash {
				t.Fatalf("name %q hash = %v, want %v", got, hasHash, tc.wantHash)
			}
			if !tc.wantHash && got != tc.namespace+"_"+tc.object {
				t.Fatalf("name = %q, want the plain join", got)
			}
		})
	}
}

// TestLongNamesAreCapped checks every family that Host compacts, including the
// longest file and unit extensions.
func TestLongNamesAreCapped(t *testing.T) {
	ns := strings.Repeat("n", 253)
	obj := strings.Repeat("o", 253)
	container := strings.Repeat("c", 253)
	entry := strings.Repeat("e", 253)
	hashRE := regexp.MustCompile(`_[0-9a-f]{16}`)
	accepted := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

	cases := []struct {
		kind  names.Kind
		name  string
		extra []string
	}{
		{names.KindPod, obj, nil},
		{names.KindPodContainer, obj, []string{container}},
		{names.KindPodFile, obj, nil},
		{names.KindPodService, obj, nil},
		{names.KindPodUnit, obj, nil},
		{names.KindPodContainerFile, obj, []string{container}},
		{names.KindPodContainerService, obj, []string{container}},
		{names.KindJob, obj, nil},
		{names.KindJobFile, obj, nil},
		{names.KindJobService, obj, nil},
		{names.KindCronJob, obj, nil},
		{names.KindCronJobFile, obj, nil},
		{names.KindCronJobService, obj, nil},
		{names.KindCronJobTimer, obj, nil},
		{names.KindEnvPod, obj, nil},
		{names.KindEnvJob, obj, nil},
		{names.KindEnvCronJob, obj, nil},
		{names.KindPVCVolume, obj, nil},
		{names.KindPVCFile, obj, nil},
		{names.KindDeploymentHost, obj, []string{"1"}},
		{names.KindSocketFile, obj, []string{entry}},
		{names.KindSocketService, obj, nil},
		{names.KindIngressFile, obj, nil},
		{names.KindMiddlewareFile, obj, nil},
		{names.KindIngressRouteTCPFile, obj, nil},
		{names.KindIngressRoute, obj, []string{"0"}},
		{names.KindIngressRouteTCP, obj, []string{"0"}},
	}
	for _, tc := range cases {
		got := names.Host(tc.kind, ns, tc.name, tc.extra...)
		if len(got) > 255 {
			t.Errorf("Host(%s) length = %d, want <= 255: %q", tc.kind, len(got), got)
		}
		if !hashRE.MatchString(got) {
			t.Errorf("Host(%s) = %q, want a hash fallback", tc.kind, got)
		}
		if !accepted.MatchString(got) {
			t.Errorf("Host(%s) = %q contains a character no consumer accepts", tc.kind, got)
		}
	}
}

// TestLongNameBasesStayConsistent checks that a truncated base and every
// artifact derived from it agree. If the hash domain or the fixed suffix were
// applied independently, a .container file and its .service unit would point
// at different Podman names.
func TestLongNameBasesStayConsistent(t *testing.T) {
	ns := strings.Repeat("n", 253)
	obj := strings.Repeat("o", 253)

	pod := names.Host(names.KindPod, ns, obj)
	if got := names.Host(names.KindPodContainer, ns, obj, ""); got != pod {
		t.Errorf("single-container name = %q, want the Pod base %q", got, pod)
	}
	if got, want := names.Host(names.KindPodContainerFile, ns, obj, ""), pod+".container"; got != want {
		t.Errorf("single-container file = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindPodContainerService, ns, obj, ""), pod+".service"; got != want {
		t.Errorf("single-container service = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindPodFile, ns, obj), pod+".pod"; got != want {
		t.Errorf("Pod file = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindPodService, ns, obj), pod+".service"; got != want {
		t.Errorf("Pod service = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindPodUnit, ns, obj), pod+"-pod.service"; got != want {
		t.Errorf("Pod unit = %q, want %q", got, want)
	}

	pvc := names.Host(names.KindPVCVolume, ns, obj)
	if got, want := names.Host(names.KindPVCFile, ns, obj), pvc+".volume"; got != want {
		t.Errorf("PVC file = %q, want %q", got, want)
	}

	job := names.Host(names.KindJob, ns, obj)
	if got, want := names.Host(names.KindJobFile, ns, obj), job+".container"; got != want {
		t.Errorf("Job file = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindJobService, ns, obj), job+".service"; got != want {
		t.Errorf("Job service = %q, want %q", got, want)
	}

	cron := names.Host(names.KindCronJob, ns, obj)
	if got, want := names.Host(names.KindCronJobFile, ns, obj), cron+".container"; got != want {
		t.Errorf("CronJob file = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindCronJobService, ns, obj), cron+".service"; got != want {
		t.Errorf("CronJob service = %q, want %q", got, want)
	}
	if got, want := names.Host(names.KindCronJobTimer, ns, obj), cron+".timer"; got != want {
		t.Errorf("CronJob timer = %q, want %q", got, want)
	}
}

func TestHostIsDeterministic(t *testing.T) {
	for _, tc := range kindCases() {
		first := names.Host(tc.kind, tc.namespace, tc.name, tc.extra...)
		for i := 0; i < 5; i++ {
			if got := names.Host(tc.kind, tc.namespace, tc.name, tc.extra...); got != first {
				t.Fatalf("Host(%s) changed between calls: %q then %q", tc.kind, first, got)
			}
		}
	}
}

func TestCompactDifferentTuplesDiffer(t *testing.T) {
	long := strings.Repeat("p", 253)
	a := names.Compact(names.CompactSpec{Family: names.KindPod}, long, long)
	b := names.Compact(names.CompactSpec{Family: names.KindPod}, long, long+"x")
	c := names.Compact(names.CompactSpec{Family: names.KindPod}, long+"x", long)
	if a == b || a == c || b == c {
		t.Fatalf("different long tuples collided: %q, %q, %q", a, b, c)
	}
}

// TestShortNamesAreReversible recovers the namespace and object name from the
// new <ns>_<name> form. It is valid only below the truncation threshold, which
// the design accepts because the hash fallback keeps long names distinct even
// though they are no longer decodable.
func TestShortNamesAreReversible(t *testing.T) {
	pairs := [][2]string{
		{"team", "prod-db"},
		{"team-prod", "db"},
		{"a-b", "c-d"},
		{"x1", "y2"},
	}
	for _, pair := range pairs {
		for _, kind := range []names.Kind{names.KindPod, names.KindPVCVolume} {
			got := names.Host(kind, pair[0], pair[1])
			namespace, name, ok := strings.Cut(got, "_")
			if !ok || namespace != pair[0] || name != pair[1] {
				t.Errorf("Host(%s, %q, %q) = %q is not reversible", kind, pair[0], pair[1], got)
			}
			if strings.Contains(name, "_") {
				t.Errorf("Host(%s, %q, %q) = %q has more than one separator", kind, pair[0], pair[1], got)
			}
		}
	}
}

// TestAppendixAMatrix checks the collision matrix from Appendix A. Every tuple
// there must map to a distinct new name.
func TestAppendixAMatrix(t *testing.T) {
	type tuple struct {
		namespace string
		name      string
		kind      names.Kind
		extra     []string
	}
	matrix := []tuple{
		{"team", "prod-db", names.KindPod, nil},
		{"team-prod", "db", names.KindPod, nil},
		{"team", "x", names.KindJob, nil},
		{"team", "x-job", names.KindPod, nil},
		{"team", "x", names.KindCronJob, nil},
		{"team", "x-cron", names.KindPod, nil},
		{"team", "a", names.KindPodContainer, []string{"b-c"}},
		{"team", "a-b", names.KindPodContainer, []string{"c"}},
	}
	seen := make(map[string]tuple)
	for _, tc := range matrix {
		got := names.Host(tc.kind, tc.namespace, tc.name, tc.extra...)
		if previous, ok := seen[got]; ok {
			t.Errorf("%s/%s %s and %s/%s %s both became %q",
				tc.namespace, tc.name, tc.kind,
				previous.namespace, previous.name, previous.kind, got)
		}
		seen[got] = tc
	}
}

func TestEffectivePVCNames(t *testing.T) {
	const ns, claim = "team", "data"
	legacy := names.LegacyHost(names.KindPVCVolume, ns, claim)
	current := names.Host(names.KindPVCVolume, ns, claim)

	cases := []struct {
		name         string
		persisted    string
		legacyFile   bool
		wantVolume   string
		wantFileBase string
	}{
		{"legacy claim keeps its volume forever", legacy, false, legacy, legacy},
		{"new claim keeps its volume", current, false, current, current},
		{"unbound claim adopts a legacy file", "", true, legacy, legacy},
		{"unbound claim without an old file is new", "", false, current, current},
		{"a foreign persisted binding is ignored", "custom-volume", false, current, current},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := names.EffectivePVCVolumeName(ns, claim, tc.persisted, tc.legacyFile); got != tc.wantVolume {
				t.Errorf("EffectivePVCVolumeName = %q, want %q", got, tc.wantVolume)
			}
			if got := names.EffectivePVCFileBase(ns, claim, tc.persisted, tc.legacyFile); got != tc.wantFileBase {
				t.Errorf("EffectivePVCFileBase = %q, want %q", got, tc.wantFileBase)
			}
		})
	}
}

func TestMissingExtraPanics(t *testing.T) {
	cases := []struct {
		name string
		call func()
	}{
		{"unknown kind", func() { names.Host("not-a-kind", "team", "x") }},
		{"deployment without index", func() { names.Host(names.KindDeploymentHost, "team", "web") }},
		{"socket without entry", func() { names.Host(names.KindSocketFile, "team", "app") }},
		{"emptyDir without volume", func() { names.Host(names.KindEmptyDirVolume, "team", "app") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", tc.name)
				}
			}()
			tc.call()
		})
	}
}

// TestEffectivePVCNameIgnoresForeignVolumeName: spec.volumeName is client
// writable, so a value naming another claim's volume must not be used.
func TestEffectivePVCNameIgnoresForeignVolumeName(t *testing.T) {
	for _, foreign := range []string{"other-db", "other_db", "team-prod-db", "q8s_team_x_y", "../etc"} {
		got := names.EffectivePVCVolumeName("team", "db", foreign, false)
		if got != names.Host(names.KindPVCVolume, "team", "db") {
			t.Errorf("volumeName %q: got %q, want the claim's own new name", foreign, got)
		}
		if got := names.EffectivePVCFileBase("team", "db", foreign, true); got != names.LegacyHost(names.KindPVCVolume, "team", "db") {
			t.Errorf("volumeName %q with legacy file: got %q, want the legacy name", foreign, got)
		}
	}
	if got := names.EffectivePVCVolumeName("team", "db", "team-db", false); got != "team-db" {
		t.Errorf("legacy volumeName not kept: %q", got)
	}
}
