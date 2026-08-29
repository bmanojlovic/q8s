package install

import "testing"

func TestCertsNeedRegen(t *testing.T) {
	cases := []struct {
		name     string
		exist    bool
		force    bool
		merged   PersistentConfig
		existing PersistentConfig
		want     bool
	}{
		{"missing certs", false, false, PersistentConfig{}, PersistentConfig{}, true},
		{"force", true, true, PersistentConfig{}, PersistentConfig{}, true},
		{"first install with SANs", true, false,
			PersistentConfig{ExtraSANDNS: []string{"a.example"}}, PersistentConfig{}, true},
		{"unchanged SANs", true, false,
			PersistentConfig{ExtraSANDNS: []string{"a.example"}}, PersistentConfig{ExtraSANDNS: []string{"a.example"}}, false},
		{"new SAN added", true, false,
			PersistentConfig{ExtraSANDNS: []string{"a.example", "b.example"}}, PersistentConfig{ExtraSANDNS: []string{"a.example"}}, true},
		{"new IP SAN added", true, false,
			PersistentConfig{ExtraSANIPs: []string{"10.0.0.1"}}, PersistentConfig{}, true},
		{"no SANs at all", true, false, PersistentConfig{}, PersistentConfig{}, false},
	}
	for _, tc := range cases {
		if got := certsNeedRegen(tc.exist, tc.force, tc.merged, tc.existing); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSameStrings(t *testing.T) {
	if !sameStrings([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("expected set-equal")
	}
	if sameStrings([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("expected not equal")
	}
	if sameStrings([]string{"a", "b"}, []string{"a", "b", "c"}) {
		t.Error("expected not equal (different length)")
	}
	if sameStrings(nil, []string{"a"}) {
		t.Error("expected not equal (nil vs one)")
	}
}
