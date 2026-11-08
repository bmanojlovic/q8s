package hostcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testContainersLine = "containers:2147483647:2147483648"

func writeTempFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTempFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPlanContainersRange(t *testing.T) {
	tests := []struct {
		name    string
		subuid  string
		subgid  string
		wantUID bool
		wantGID bool
		wantErr string // substring
	}{
		{
			name:    "both missing",
			subuid:  "alice:100000:65536\n",
			subgid:  "alice:100000:65536\n",
			wantUID: true,
			wantGID: true,
		},
		{
			name:    "both present",
			subuid:  testContainersLine + "\n",
			subgid:  testContainersLine + "\n",
			wantUID: false,
			wantGID: false,
		},
		{
			name:    "only gid missing",
			subuid:  testContainersLine + "\n",
			subgid:  "",
			wantUID: false,
			wantGID: true,
		},
		{
			name:    "larger range is enough",
			subuid:  "containers:100000:70000\n",
			subgid:  "containers:100000:70000\n",
			wantUID: false,
			wantGID: false,
		},
		{
			name:    "too small uid",
			subuid:  "containers:100000:1000\n",
			subgid:  "",
			wantErr: "fewer than 65536 ids",
		},
		{
			name:    "too small gid",
			subuid:  "",
			subgid:  "containers:100000:1000\n",
			wantErr: "fewer than 65536 ids",
		},
		{
			name:    "overlapping uid",
			subuid:  "alice:2000000000:200000000\n",
			subgid:  "",
			wantErr: "alice:2000000000:200000000",
		},
		{
			name:    "overlapping gid",
			subuid:  "",
			subgid:  "bob:4000000000:400000000\n",
			wantErr: "bob:4000000000:400000000",
		},
		{
			name:    "range ending exactly at the start does not overlap",
			subuid:  "alice:100000:2047383647\n",
			subgid:  "alice:100000:2047383647\n",
			wantUID: true,
			wantGID: true,
		},
		{
			name:    "range starting at the end does not overlap",
			subuid:  "alice:4294967295:65536\n",
			subgid:  "alice:4294967295:65536\n",
			wantUID: true,
			wantGID: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addUID, addGID, err := PlanContainersRange([]byte(tc.subuid), []byte(tc.subgid))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				if addUID || addGID {
					t.Errorf("plan with an error must not ask for writes (uid=%v gid=%v)", addUID, addGID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addUID != tc.wantUID || addGID != tc.wantGID {
				t.Errorf("plan = (uid=%v, gid=%v), want (uid=%v, gid=%v)", addUID, addGID, tc.wantUID, tc.wantGID)
			}
		})
	}
}

func TestEnsureContainersRangeAppendsAndKeepsExistingBytes(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	uidData := "alice:100000:65536\n# keep me\nother:200000:65536\n"
	gidData := "alice:100000:65536\n"
	writeTempFile(t, subuid, uidData)
	writeTempFile(t, subgid, gidData)

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err != nil {
		t.Fatalf("EnsureContainersRange: %v", err)
	}
	if !changedUID || !changedGID {
		t.Fatalf("changed = (uid=%v, gid=%v), want both true", changedUID, changedGID)
	}

	gotUID := readTempFile(t, subuid)
	if gotUID != uidData+testContainersLine+"\n" {
		t.Errorf("subuid = %q, want existing bytes verbatim plus the appended line", gotUID)
	}
	gotGID := readTempFile(t, subgid)
	if gotGID != gidData+testContainersLine+"\n" {
		t.Errorf("subgid = %q, want existing bytes verbatim plus the appended line", gotGID)
	}
}

func TestEnsureContainersRangeAlreadyPresentDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	uidData := testContainersLine + "\n"
	gidData := testContainersLine + "\n"
	writeTempFile(t, subuid, uidData)
	writeTempFile(t, subgid, gidData)

	beforeUID, err := os.Stat(subuid)
	if err != nil {
		t.Fatal(err)
	}
	beforeGID, err := os.Stat(subgid)
	if err != nil {
		t.Fatal(err)
	}

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err != nil {
		t.Fatalf("EnsureContainersRange: %v", err)
	}
	if changedUID || changedGID {
		t.Fatalf("changed = (uid=%v, gid=%v), want none", changedUID, changedGID)
	}
	if got := readTempFile(t, subuid); got != uidData {
		t.Errorf("subuid changed to %q", got)
	}
	if got := readTempFile(t, subgid); got != gidData {
		t.Errorf("subgid changed to %q", got)
	}
	afterUID, err := os.Stat(subuid)
	if err != nil {
		t.Fatal(err)
	}
	afterGID, err := os.Stat(subgid)
	if err != nil {
		t.Fatal(err)
	}
	if !afterUID.ModTime().Equal(beforeUID.ModTime()) {
		t.Errorf("subuid mtime changed, file was touched")
	}
	if !afterGID.ModTime().Equal(beforeGID.ModTime()) {
		t.Errorf("subgid mtime changed, file was touched")
	}
}

func TestEnsureContainersRangeTooSmallLeavesFilesUnchanged(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	uidData := "containers:100000:1000\n"
	gidData := "containers:100000:1000\n"
	writeTempFile(t, subuid, uidData)
	writeTempFile(t, subgid, gidData)

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err == nil || !strings.Contains(err.Error(), "fewer than 65536 ids") {
		t.Fatalf("error = %v, want a too-small error", err)
	}
	if changedUID || changedGID {
		t.Errorf("changed = (uid=%v, gid=%v), want none", changedUID, changedGID)
	}
	if got := readTempFile(t, subuid); got != uidData {
		t.Errorf("subuid changed to %q", got)
	}
	if got := readTempFile(t, subgid); got != gidData {
		t.Errorf("subgid changed to %q", got)
	}
}

func TestEnsureContainersRangeOverlapLeavesFilesUnchanged(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	writeTempFile(t, subuid, "alice:2000000000:200000000\n")
	writeTempFile(t, subgid, "alice:100000:65536\n")

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err == nil || !strings.Contains(err.Error(), "overlaps existing entry alice:2000000000:200000000") {
		t.Fatalf("error = %v, want an overlap error naming the entry", err)
	}
	if changedUID || changedGID {
		t.Errorf("changed = (uid=%v, gid=%v), want none", changedUID, changedGID)
	}
	if got := readTempFile(t, subuid); got != "alice:2000000000:200000000\n" {
		t.Errorf("subuid changed to %q", got)
	}
	if got := readTempFile(t, subgid); got != "alice:100000:65536\n" {
		t.Errorf("subgid changed to %q", got)
	}
}

func TestEnsureContainersRangeCreatesMissingFiles(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err != nil {
		t.Fatalf("EnsureContainersRange: %v", err)
	}
	if !changedUID || !changedGID {
		t.Fatalf("changed = (uid=%v, gid=%v), want both true", changedUID, changedGID)
	}
	for _, path := range []string{subuid, subgid} {
		if got := readTempFile(t, path); got != testContainersLine+"\n" {
			t.Errorf("%s = %q, want just the containers line", path, got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0644 {
			t.Errorf("%s mode = %o, want 644", path, perm)
		}
	}
}

func TestEnsureContainersRangeAddsNewlineBeforeAppending(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	writeTempFile(t, subuid, "alice:100000:65536")
	writeTempFile(t, subgid, "alice:100000:65536")

	if _, _, err := EnsureContainersRange(subuid, subgid); err != nil {
		t.Fatalf("EnsureContainersRange: %v", err)
	}
	want := "alice:100000:65536\n" + testContainersLine + "\n"
	if got := readTempFile(t, subuid); got != want {
		t.Errorf("subuid = %q, want %q", got, want)
	}
	if got := readTempFile(t, subgid); got != want {
		t.Errorf("subgid = %q, want %q", got, want)
	}
}

func TestEnsureContainersRangeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	writeTempFile(t, subuid, "alice:100000:65536\n")
	writeTempFile(t, subgid, "alice:100000:65536\n")

	if _, _, err := EnsureContainersRange(subuid, subgid); err != nil {
		t.Fatalf("first call: %v", err)
	}
	uidAfterFirst := readTempFile(t, subuid)
	gidAfterFirst := readTempFile(t, subgid)

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if changedUID || changedGID {
		t.Errorf("second call changed files: uid=%v gid=%v", changedUID, changedGID)
	}
	if got := readTempFile(t, subuid); got != uidAfterFirst {
		t.Errorf("subuid changed on the second call: %q", got)
	}
	if got := readTempFile(t, subgid); got != gidAfterFirst {
		t.Errorf("subgid changed on the second call: %q", got)
	}
}

func TestEnsureContainersRangeOnlyAppendsMissingSide(t *testing.T) {
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	uidData := testContainersLine + "\n"
	writeTempFile(t, subuid, uidData)
	writeTempFile(t, subgid, "")

	changedUID, changedGID, err := EnsureContainersRange(subuid, subgid)
	if err != nil {
		t.Fatalf("EnsureContainersRange: %v", err)
	}
	if changedUID || !changedGID {
		t.Fatalf("changed = (uid=%v, gid=%v), want only gid", changedUID, changedGID)
	}
	if got := readTempFile(t, subuid); got != uidData {
		t.Errorf("subuid changed to %q", got)
	}
	if got := readTempFile(t, subgid); got != testContainersLine+"\n" {
		t.Errorf("subgid = %q", got)
	}
}
