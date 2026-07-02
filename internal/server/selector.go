package server

import "strings"

// selOp is a label-selector requirement operator.
type selOp int

const (
	selEquals selOp = iota
	selNotEquals
	selExists
	selNotExists
)

type labelRequirement struct {
	key   string
	value string
	op    selOp
}

// parseLabelSelector parses the common subset of k8s label selector syntax:
// a comma-separated list of "key=value" / "key==value" (equality),
// "key!=value" (inequality), "key" (existence), and "!key"
// (non-existence). Set-based selectors ("key in (a,b)", "key notin (a,b)")
// aren't supported — this covers what `kubectl get -l ...` actually sends
// in the overwhelming majority of real usage.
func parseLabelSelector(sel string) []labelRequirement {
	if sel == "" {
		return nil
	}
	var reqs []labelRequirement
	for _, part := range strings.Split(sel, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, "!="); idx >= 0 {
			reqs = append(reqs, labelRequirement{
				key: strings.TrimSpace(part[:idx]), value: strings.TrimSpace(part[idx+2:]), op: selNotEquals,
			})
			continue
		}
		if strings.HasPrefix(part, "!") {
			reqs = append(reqs, labelRequirement{key: strings.TrimSpace(part[1:]), op: selNotExists})
			continue
		}
		if idx := strings.Index(part, "=="); idx >= 0 {
			reqs = append(reqs, labelRequirement{
				key: strings.TrimSpace(part[:idx]), value: strings.TrimSpace(part[idx+2:]), op: selEquals,
			})
			continue
		}
		if idx := strings.Index(part, "="); idx >= 0 {
			reqs = append(reqs, labelRequirement{
				key: strings.TrimSpace(part[:idx]), value: strings.TrimSpace(part[idx+1:]), op: selEquals,
			})
			continue
		}
		reqs = append(reqs, labelRequirement{key: part, op: selExists})
	}
	return reqs
}

// matchesEqualitySelector reports whether labels satisfies selector, the
// plain equality-map form used by Service.Spec.Selector (every key must be
// present with an equal value). An empty/nil selector never matches — a
// Service with no selector doesn't automatically own every pod, same as
// real k8s (it's the "manually managed endpoints" case).
func matchesEqualitySelector(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func matchesSelector(labels map[string]string, reqs []labelRequirement) bool {
	for _, req := range reqs {
		v, ok := labels[req.key]
		switch req.op {
		case selEquals:
			if !ok || v != req.value {
				return false
			}
		case selNotEquals:
			if ok && v == req.value {
				return false
			}
		case selExists:
			if !ok {
				return false
			}
		case selNotExists:
			if ok {
				return false
			}
		}
	}
	return true
}
