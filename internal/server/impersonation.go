package server

import (
	"fmt"
	"net/http"
	"strings"
)

// impersonationError describes a rejected Impersonate-* request. forbidden
// distinguishes "you may not impersonate" (403) from a malformed request
// (400).
type impersonationError struct {
	message   string
	forbidden bool
}

// applyImpersonation replaces the authenticated identity with the one named
// by the Impersonate-User and Impersonate-Group headers. Only administrators
// may impersonate. Impersonate-Uid is accepted but ignored, as q8s identities
// carry no UID.
func applyImpersonation(r *http.Request, u userInfo) (userInfo, *impersonationError) {
	userValues, hasUser := headerValues(r.Header, "Impersonate-User")
	groupValues, hasGroup := headerValues(r.Header, "Impersonate-Group")
	hasAny := hasImpersonationHeader(r.Header)

	if !hasAny {
		return u, nil
	}

	if !isAdmin(u) {
		target := ""
		resource := "users"
		if len(userValues) > 0 && userValues[0] != "" {
			target = userValues[0]
		} else if len(groupValues) > 0 && groupValues[0] != "" {
			target = groupValues[0]
			resource = "groups"
		} else if uid, ok := firstHeaderValue(r.Header, "Impersonate-Uid"); ok {
			target = uid
			resource = "uids"
		}
		return u, &impersonationError{
			forbidden: true,
			message: fmt.Sprintf("%s %q is forbidden: User %q cannot impersonate resource %q in API group \"\" at the cluster scope",
				resource, target, u.Name, resource),
		}
	}

	if !hasUser {
		if hasGroup {
			return u, &impersonationError{message: "Impersonate-Group requires Impersonate-User"}
		}
		// Impersonate-Uid alone is ignored.
		return u, nil
	}
	if len(userValues) == 0 || userValues[0] == "" {
		return u, &impersonationError{message: "Impersonate-User must not be empty"}
	}

	name := userValues[0]
	groups := make([]string, 0, len(groupValues)+1)
	seen := make(map[string]bool, len(groupValues)+1)
	for _, group := range groupValues {
		if group == "" || seen[group] {
			continue
		}
		seen[group] = true
		groups = append(groups, group)
	}
	if !seen["system:authenticated"] {
		groups = append(groups, "system:authenticated")
	}
	return userInfo{Name: name, Groups: groups}, nil
}

// hasImpersonationHeader reports whether any Impersonate-* header is present,
// including an empty one.
func hasImpersonationHeader(header http.Header) bool {
	for key := range header {
		if strings.HasPrefix(strings.ToLower(key), "impersonate-") {
			return true
		}
	}
	return false
}

// headerValues returns every value of the named header and whether the header
// was present at all.
func headerValues(header http.Header, name string) ([]string, bool) {
	for key, values := range header {
		if strings.EqualFold(key, name) {
			return values, true
		}
	}
	return nil, false
}

func firstHeaderValue(header http.Header, name string) (string, bool) {
	values, ok := headerValues(header, name)
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}
