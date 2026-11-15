package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func serviceAccountBody(ns, name string) map[string]interface{} {
	metadata := map[string]interface{}{"name": name}
	if ns != "" {
		metadata["namespace"] = ns
	}
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   metadata,
	}
}

func serviceAccountCollection(ns string) string {
	if ns == "" {
		return "/api/v1/serviceaccounts"
	}
	return "/api/v1/namespaces/" + ns + "/serviceaccounts"
}

func serviceAccountURL(ns, name string) string {
	return serviceAccountCollection(ns) + "/" + name
}

func TestServiceAccountCRUD(t *testing.T) {
	ts, _ := newTestServer(t)
	collection := ts.URL + serviceAccountCollection("default")
	objectURL := ts.URL + serviceAccountURL("default", "demo")

	resp := post(t, collection, serviceAccountBody("default", "demo"))
	assertStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	assertKind(t, created, "ServiceAccount")
	metadata, _ := created["metadata"].(map[string]interface{})
	if metadata["uid"] == nil || metadata["resourceVersion"] == nil || metadata["creationTimestamp"] == nil {
		t.Fatalf("server-owned metadata missing: %v", metadata)
	}

	resp = post(t, collection, serviceAccountBody("default", "demo"))
	assertStatus(t, resp, http.StatusConflict)
	resp.Body.Close()

	resp = get(t, collection)
	assertStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	assertKind(t, list, "ServiceAccountList")
	items, _ := list["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected one item, got %v", list["items"])
	}

	resp = get(t, objectURL)
	assertStatus(t, resp, http.StatusOK)
	assertKind(t, decodeBody(t, resp), "ServiceAccount")

	resp = getWithAccept(t, collection, "application/json;as=Table;v=v1;g=meta.k8s.io, application/json")
	assertStatus(t, resp, http.StatusOK)
	table := decodeBody(t, resp)
	assertKind(t, table, "Table")
	cols := tableColumnNames(t, table)
	wantCols := []string{"Name", "Secrets", "Age"}
	if len(cols) != len(wantCols) {
		t.Fatalf("table columns = %v, want %v", cols, wantCols)
	}
	for i, want := range wantCols {
		if cols[i] != want {
			t.Fatalf("table columns = %v, want %v", cols, wantCols)
		}
	}
	row := table["rows"].([]interface{})[0].(map[string]interface{})
	cells := row["cells"].([]interface{})
	if cells[0] != "demo" {
		t.Fatalf("NAME cell = %v", cells[0])
	}
	if cells[1] != float64(0) {
		t.Fatalf("SECRETS cell = %v, want 0", cells[1])
	}

	// PUT replaces the object.
	updated := serviceAccountBody("default", "demo")
	updated["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{"updated": "yes"}
	resp = do(t, http.MethodPut, objectURL, updated)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Merge patch adds a label without dropping the existing one.
	resp = do(t, http.MethodPatch, objectURL, map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{"patched": "yes"}},
	})
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = get(t, objectURL)
	assertStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	labels, _ := got["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
	if labels["updated"] != "yes" || labels["patched"] != "yes" {
		t.Fatalf("labels after patch = %v", labels)
	}

	// Apply creates and updates through the same handler, like Terraform's
	// kubernetes_manifest does.
	applyBody := []byte("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: demo\n  labels:\n    applied: \"yes\"\n")
	req, err := http.NewRequest(http.MethodPatch, objectURL, bytes.NewReader(applyBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = get(t, objectURL)
	assertStatus(t, resp, http.StatusOK)
	got = decodeBody(t, resp)
	labels, _ = got["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
	if labels["applied"] != "yes" {
		t.Fatalf("labels after apply = %v", labels)
	}

	resp = do(t, http.MethodDelete, objectURL, nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = get(t, objectURL)
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestServiceAccountAllNamespaceList(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, target := range []struct{ ns, name string }{{"default", "one"}, {"demo", "two"}} {
		resp := post(t, ts.URL+serviceAccountCollection(target.ns), serviceAccountBody(target.ns, target.name))
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}

	resp := get(t, ts.URL+serviceAccountCollection(""))
	assertStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	assertKind(t, list, "ServiceAccountList")
	items, _ := list["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("all-namespace list has %d items, want 2: %v", len(items), items)
	}
}

func TestServiceAccountWatch(t *testing.T) {
	ts, _ := newTestServer(t)
	collection := ts.URL + serviceAccountCollection("default")
	resp := post(t, collection, serviceAccountBody("default", "watched"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	events := startWatch(t, collection+"?resourceVersion=0&watch=true", "")
	first := nextWatchEvent(t, events)
	if first.Type != "ADDED" {
		t.Fatalf("first watch event = %q, want ADDED", first.Type)
	}
	if name := first.Object["metadata"].(map[string]interface{})["name"]; name != "watched" {
		t.Fatalf("first watch event name = %v", name)
	}

	resp = post(t, collection, serviceAccountBody("default", "later"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	second := nextWatchEvent(t, events)
	if second.Type != "ADDED" {
		t.Fatalf("second watch event = %q, want ADDED", second.Type)
	}
	if name := second.Object["metadata"].(map[string]interface{})["name"]; name != "later" {
		t.Fatalf("second watch event name = %v", name)
	}
}

func TestServiceAccountNamespaceCleanup(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := post(t, ts.URL+"/api/v1/namespaces", nsBody("demo"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = post(t, ts.URL+serviceAccountCollection("demo"), serviceAccountBody("demo", "worker"))
	assertStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = do(t, http.MethodDelete, ts.URL+"/api/v1/namespaces/demo", nil)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = get(t, ts.URL+serviceAccountURL("demo", "worker"))
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
	resp = get(t, ts.URL+serviceAccountCollection(""))
	assertStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	items, _ := list["items"].([]interface{})
	if len(items) != 0 {
		body, _ := json.Marshal(items)
		t.Fatalf("service accounts survived namespace deletion: %s", body)
	}
}
