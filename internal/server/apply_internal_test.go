package server

import (
	"encoding/json"
	"testing"
)

// TestApplyPatchOps: user sections are replaced as whole values (so list
// elements and map keys the applier dropped disappear), data-like sections
// the applier omits are removed, spec is never removed, and only labels and
// annotations of metadata are touched.
func TestApplyPatchOps(t *testing.T) {
	current := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":        "demo",
			"uid":         "u1",
			"labels":      map[string]interface{}{"app": "demo", "old": "yes"},
			"annotations": map[string]interface{}{"a": "1"},
		},
		"spec":       map[string]interface{}{"replicas": float64(1)},
		"data":       map[string]interface{}{"keep": "one", "drop": "two"},
		"stringData": map[string]interface{}{"x": "y"},
		"status":     map[string]interface{}{"phase": "Running"},
	}
	applied := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":   "demo",
			"labels": map[string]interface{}{"app": "demo"},
		},
		"data": map[string]interface{}{"keep": "one"},
	}
	got, err := json.Marshal(applyPatchOps(current, applied))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"op":"add","path":"/data","value":{"keep":"one"}},` +
		`{"op":"remove","path":"/stringData"},` +
		`{"op":"add","path":"/metadata/labels","value":{"app":"demo"}},` +
		`{"op":"remove","path":"/metadata/annotations"}]`
	if string(got) != want {
		t.Fatalf("ops = %s\nwant  %s", got, want)
	}
}

// TestApplyPatchOpsReplacesLists: a container list is replaced wholesale, so
// an env var removed by the applier is gone (a merge patch would have merged
// the lists by name in the PATCH handlers).
func TestApplyPatchOpsReplacesLists(t *testing.T) {
	spec := map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{"name": "web", "env": []interface{}{}}},
	}}}
	ops := applyPatchOps(map[string]interface{}{"spec": map[string]interface{}{}, "metadata": map[string]interface{}{}}, map[string]interface{}{"spec": spec, "metadata": map[string]interface{}{}})
	if len(ops) != 1 || ops[0]["op"] != "add" || ops[0]["path"] != "/spec" {
		t.Fatalf("ops = %v", ops)
	}
}
