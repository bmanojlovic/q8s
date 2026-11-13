package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsDryRun(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		want    bool
		wantErr bool
	}{
		{name: "absent", query: "", want: false},
		{name: "All", query: "?dryRun=All", want: true},
		{name: "All twice", query: "?dryRun=All&dryRun=All", want: true},
		{name: "other value", query: "?dryRun=No", wantErr: true},
		{name: "empty value", query: "?dryRun=", wantErr: true},
		{name: "mixed values", query: "?dryRun=All&dryRun=No", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/pods"+tc.query, nil)
			got, err := isDryRun(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("isDryRun(%q) = %v, want an error", tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("isDryRun(%q): %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("isDryRun(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestDryRunWriteIgnoresReadRequests(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name   string
		method string
	}{
		{name: "GET", method: http.MethodGet},
		{name: "HEAD", method: http.MethodHead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/v1/namespaces/default/pods?dryRun=No", nil)
			rec := httptest.NewRecorder()
			dry, ok := s.dryRunWrite(rec, r)
			if !ok || dry {
				t.Errorf("dryRunWrite on %s = (%v, %v), want (false, true)", tc.method, dry, ok)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("read request got status %d, want no response written", rec.Code)
			}
		})
	}
}

func TestDryRunWriteRejectsBadValue(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/example?dryRun=No", nil)
	rec := httptest.NewRecorder()
	dry, ok := s.dryRunWrite(rec, r)
	if ok || dry {
		t.Fatalf("dryRunWrite = (%v, %v), want (false, false)", dry, ok)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
