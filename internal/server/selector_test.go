package server

import "testing"

func TestParseLabelSelectorAndMatch(t *testing.T) {
	cases := []struct {
		name     string
		selector string
		labels   map[string]string
		want     bool
	}{
		{"equals match", "app=foo", map[string]string{"app": "foo"}, true},
		{"equals mismatch", "app=foo", map[string]string{"app": "bar"}, false},
		{"equals missing key", "app=foo", map[string]string{}, false},
		{"double-equals match", "app==foo", map[string]string{"app": "foo"}, true},
		{"not-equals match (different value)", "app!=foo", map[string]string{"app": "bar"}, true},
		{"not-equals fails (same value)", "app!=foo", map[string]string{"app": "foo"}, false},
		{"not-equals matches missing key", "app!=foo", map[string]string{}, true},
		{"exists match", "app", map[string]string{"app": "anything"}, true},
		{"exists fails when missing", "app", map[string]string{}, false},
		{"not-exists match", "!app", map[string]string{}, true},
		{"not-exists fails when present", "!app", map[string]string{"app": "x"}, false},
		{"multiple requirements all match", "app=foo,tier=web", map[string]string{"app": "foo", "tier": "web"}, true},
		{"multiple requirements one fails", "app=foo,tier=web", map[string]string{"app": "foo", "tier": "db"}, false},
		{"empty selector matches everything", "", map[string]string{}, true},
		{"whitespace around terms", " app = foo , tier = web ", map[string]string{"app": "foo", "tier": "web"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := parseLabelSelector(tc.selector)
			got := matchesSelector(tc.labels, reqs)
			if got != tc.want {
				t.Errorf("selector %q against %v: got %v, want %v", tc.selector, tc.labels, got, tc.want)
			}
		})
	}
}

func TestMatchesEqualitySelector(t *testing.T) {
	cases := []struct {
		name     string
		labels   map[string]string
		selector map[string]string
		want     bool
	}{
		{"single key match", map[string]string{"app": "foo"}, map[string]string{"app": "foo"}, true},
		{"single key mismatch", map[string]string{"app": "bar"}, map[string]string{"app": "foo"}, false},
		{"missing key", map[string]string{}, map[string]string{"app": "foo"}, false},
		{"multi key all match", map[string]string{"app": "foo", "tier": "web"}, map[string]string{"app": "foo", "tier": "web"}, true},
		{"multi key one mismatch", map[string]string{"app": "foo", "tier": "db"}, map[string]string{"app": "foo", "tier": "web"}, false},
		{"extra pod labels ignored", map[string]string{"app": "foo", "extra": "x"}, map[string]string{"app": "foo"}, true},
		{"nil selector never matches", map[string]string{"app": "foo"}, nil, false},
		{"empty selector never matches", map[string]string{"app": "foo"}, map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchesEqualitySelector(tc.labels, tc.selector)
			if got != tc.want {
				t.Errorf("matchesEqualitySelector(%v, %v): got %v, want %v", tc.labels, tc.selector, got, tc.want)
			}
		})
	}
}
