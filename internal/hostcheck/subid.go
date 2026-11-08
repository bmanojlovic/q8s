package hostcheck

import (
	"fmt"
	"os"
)

// The range q8s hands to podman's --userns=auto. It sits above the ranges
// login users normally get (100000:65536) and is large enough that podman can
// carve a separate 65536-id slice out of it for every pod without running
// out. The same start is used in /etc/subuid and /etc/subgid.
const (
	ContainersRangeStart = 2147483647
	ContainersRangeCount = 2147483648
)

const containersEntryName = "containers"

// containersRangeLine is the line q8s appends to both subordinate id files.
func containersRangeLine() string {
	return fmt.Sprintf("%s:%d:%d", containersEntryName, ContainersRangeStart, ContainersRangeCount)
}

// PlanContainersRange is pure: given the current file contents it says what
// to append. Per file, a 'containers' entry with at least 65536 ids means
// there is nothing to do; an existing smaller entry is an admin decision q8s
// must not silently rewrite, so it is an error. A missing entry is added
// unless the suggested range overlaps any existing entry in that file.
func PlanContainersRange(subuid, subgid []byte) (addUID, addGID bool, err error) {
	addUID, err = planContainersRange(subuid)
	if err != nil {
		return false, false, fmt.Errorf("subuid: %w", err)
	}
	addGID, err = planContainersRange(subgid)
	if err != nil {
		return false, false, fmt.Errorf("subgid: %w", err)
	}
	return addUID, addGID, nil
}

func planContainersRange(data []byte) (bool, error) {
	entries := parseSubIDs(data)

	hasContainers := false
	for _, e := range entries {
		if e.Name != containersEntryName {
			continue
		}
		hasContainers = true
		if e.Count >= minSubIDCount {
			return false, nil
		}
	}
	if hasContainers {
		return false, fmt.Errorf("existing 'containers' entry has fewer than %d ids; adjust it by hand", minSubIDCount)
	}

	start := uint64(ContainersRangeStart)
	end := start + uint64(ContainersRangeCount)
	for _, e := range entries {
		if e.Count == 0 {
			continue
		}
		// Overlap of [start, end) with [e.Start, e.Start+e.Count). The
		// comparison avoids computing e.Start+e.Count when it could wrap.
		overlaps := false
		if e.Start <= start {
			overlaps = e.Count > start-e.Start
		} else {
			overlaps = e.Start < end
		}
		if overlaps {
			return false, fmt.Errorf("containers range %s overlaps existing entry %s:%d:%d",
				containersRangeLine(), e.Name, e.Start, e.Count)
		}
	}
	return true, nil
}

// EnsureContainersRange applies the plan by appending the missing line to the
// files. Existing lines are never rewritten, reordered or removed: this runs
// as root on a live host and the files also hold the login users' ranges.
// The returned booleans report which files were changed.
func EnsureContainersRange(subuidPath, subgidPath string) (changedUID, changedGID bool, err error) {
	subuid, err := readSubIDFile(subuidPath)
	if err != nil {
		return false, false, err
	}
	subgid, err := readSubIDFile(subgidPath)
	if err != nil {
		return false, false, err
	}

	addUID, addGID, err := PlanContainersRange(subuid, subgid)
	if err != nil {
		return false, false, err
	}

	line := containersRangeLine()
	if addUID {
		if err := appendSubIDLine(subuidPath, subuid, line); err != nil {
			return false, false, fmt.Errorf("%s: %w", subuidPath, err)
		}
		changedUID = true
	}
	if addGID {
		if err := appendSubIDLine(subgidPath, subgid, line); err != nil {
			return changedUID, false, fmt.Errorf("%s: %w", subgidPath, err)
		}
		changedGID = true
	}
	return changedUID, changedGID, nil
}

// readSubIDFile treats a missing file as empty: subuid/subgid do not always
// exist on a fresh host, and appending creates them.
func readSubIDFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// appendSubIDLine appends one line with O_APPEND so a concurrent write by
// another tool cannot interleave inside the line q8s writes. existing is the
// content read before planning, used only to add the separator newline when
// the file did not end in one.
func appendSubIDLine(path string, existing []byte, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	payload := ""
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		payload = "\n"
	}
	payload += line + "\n"
	if _, err := f.WriteString(payload); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
