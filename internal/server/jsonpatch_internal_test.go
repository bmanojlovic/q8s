package server

import (
	"reflect"
	"testing"
)

func TestApplyJSONPatchAddReplaceRemove(t *testing.T) {
	doc := map[string]interface{}{
		"data": map[string]interface{}{"a": "1", "b": "2"},
		"list": []interface{}{"x", "y"},
	}
	patch := []map[string]interface{}{
		{"op": "add", "path": "/data/c", "value": "3"},
		{"op": "replace", "path": "/data/a", "value": "10"},
		{"op": "remove", "path": "/data/b"},
		{"op": "add", "path": "/list/-", "value": "z"},
		{"op": "remove", "path": "/list/0"},
	}
	if err := applyJSONPatch(doc, patch); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if data := doc["data"].(map[string]interface{}); !reflect.DeepEqual(data, map[string]interface{}{"a": "10", "c": "3"}) {
		t.Fatalf("unexpected data: %v", data)
	}
	if !reflect.DeepEqual(doc["list"], []interface{}{"y", "z"}) {
		t.Fatalf("unexpected list: %v", doc["list"])
	}
}

func TestApplyJSONPatchMoveCopyTest(t *testing.T) {
	doc := map[string]interface{}{"data": map[string]interface{}{"a": "1", "b": "2"}}
	patch := []map[string]interface{}{
		{"op": "test", "path": "/data/a", "value": "1"},
		{"op": "copy", "from": "/data/a", "path": "/data/copy"},
		{"op": "move", "from": "/data/b", "path": "/data/moved"},
	}
	if err := applyJSONPatch(doc, patch); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := map[string]interface{}{"a": "1", "copy": "1", "moved": "2"}
	if !reflect.DeepEqual(doc["data"], want) {
		t.Fatalf("unexpected data: %v", doc["data"])
	}
}

func TestApplyJSONPatchErrors(t *testing.T) {
	cases := [][]map[string]interface{}{
		{{"op": "remove", "path": "/data/nope"}},
		{{"op": "replace", "path": "/data/nope", "value": "x"}},
		{{"op": "bogus", "path": "/data/a"}},
		{{"op": "test", "path": "/data/a", "value": "wrong"}},
		{{"op": "add", "path": "data/a", "value": "x"}},
		{{"op": "add", "path": "/data/a~2", "value": "x"}},
	}
	for i, patch := range cases {
		doc := map[string]interface{}{"data": map[string]interface{}{"a": "1"}}
		if err := applyJSONPatch(doc, patch); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestApplyJSONPatchEscapedKeys(t *testing.T) {
	doc := map[string]interface{}{"data": map[string]interface{}{}}
	patch := []map[string]interface{}{
		{"op": "add", "path": "/data/a~1b", "value": "slash"},
		{"op": "add", "path": "/data/t~0", "value": "tilde"},
	}
	if err := applyJSONPatch(doc, patch); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data := doc["data"].(map[string]interface{})
	if data["a/b"] != "slash" || data["t~"] != "tilde" {
		t.Fatalf("unexpected data: %v", data)
	}
}
