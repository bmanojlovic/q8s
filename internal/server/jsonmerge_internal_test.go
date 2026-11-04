package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decodeJSONMap(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return m
}

// assertNoDirectiveKeys walks v and fails if any object key starts with "$".
func assertNoDirectiveKeys(t *testing.T, v interface{}) {
	t.Helper()
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if strings.HasPrefix(k, "$") {
				t.Fatalf("directive key %q leaked into merged object", k)
			}
			assertNoDirectiveKeys(t, child)
		}
	case []interface{}:
		for _, child := range x {
			assertNoDirectiveKeys(t, child)
		}
	}
}

func TestJSONMergeStrategicDirectives(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		overlay string
		want    string
	}{
		{
			// The exact body kubectl apply sends when a sidecar container is
			// removed from a Deployment.
			name: "delete container with setElementOrder",
			base: `{
				"apiVersion":"apps/v1","kind":"Deployment",
				"metadata":{"name":"web","namespace":"default"},
				"spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},
					"template":{"metadata":{"labels":{"app":"web"}},
						"spec":{"containers":[
							{"name":"web","image":"nginx"},
							{"name":"sidecar","image":"busybox"}
						]}}}}`,
			overlay: `{"spec":{"template":{"spec":{
				"$setElementOrder/containers":[{"name":"web"}],
				"containers":[{"$patch":"delete","name":"sidecar"}]}}}}`,
			want: `{
				"apiVersion":"apps/v1","kind":"Deployment",
				"metadata":{"name":"web","namespace":"default"},
				"spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},
					"template":{"metadata":{"labels":{"app":"web"}},
						"spec":{"containers":[{"name":"web","image":"nginx"}]}}}}`,
		},
		{
			name: "delete env var keeps others in order",
			base: `{"spec":{"template":{"spec":{"containers":[
				{"name":"web","image":"nginx","env":[
					{"name":"A","value":"1"},
					{"name":"B","value":"2"},
					{"name":"C","value":"3"}
				]}]}}}}`,
			overlay: `{"spec":{"template":{"spec":{"containers":[
				{"name":"web","env":[{"$patch":"delete","name":"B"}]}]}}}}`,
			want: `{"spec":{"template":{"spec":{"containers":[
				{"name":"web","image":"nginx","env":[
					{"name":"A","value":"1"},
					{"name":"C","value":"3"}
				]}]}}}}`,
		},
		{
			name:    "setElementOrder reorders",
			base:    `{"spec":{"containers":[{"name":"a"},{"name":"b"},{"name":"c"}]}}`,
			overlay: `{"spec":{"$setElementOrder/containers":[{"name":"c"},{"name":"a"}]}}`,
			want:    `{"spec":{"containers":[{"name":"c"},{"name":"a"},{"name":"b"}]}}`,
		},
		{
			name:    "retainKeys drops unlisted keys",
			base:    `{"metadata":{"name":"x","namespace":"default","labels":{"a":"b"}}}`,
			overlay: `{"metadata":{"$retainKeys":["name"],"name":"x"}}`,
			want:    `{"metadata":{"name":"x"}}`,
		},
		{
			name:    "patch replace on a list",
			base:    `{"spec":{"containers":[{"name":"a"},{"name":"b"}]}}`,
			overlay: `{"spec":{"containers":[{"$patch":"replace"},{"name":"x","image":"img"}]}}`,
			want:    `{"spec":{"containers":[{"name":"x","image":"img"}]}}`,
		},
		{
			name:    "patch replace on a map",
			base:    `{"spec":{"replicas":2,"paused":true,"selector":{"a":"b"}}}`,
			overlay: `{"spec":{"$patch":"replace","replicas":1,"paused":false}}`,
			want:    `{"spec":{"replicas":1,"paused":false}}`,
		},
		{
			name:    "deleteFromPrimitiveList removes values",
			base:    `{"spec":{"containers":[{"name":"web","args":["a","b","c"]}]}}`,
			overlay: `{"spec":{"containers":[{"name":"web","$deleteFromPrimitiveList/args":["b"]}]}}`,
			want:    `{"spec":{"containers":[{"name":"web","args":["a","c"]}]}}`,
		},
		{
			name:    "delete of non-existent name is a no-op",
			base:    `{"spec":{"containers":[{"name":"web","image":"nginx"}]}}`,
			overlay: `{"spec":{"containers":[{"$patch":"delete","name":"ghost"}]}}`,
			want:    `{"spec":{"containers":[{"name":"web","image":"nginx"}]}}`,
		},
		{
			name:    "patch delete on a map removes the parent key",
			base:    `{"metadata":{"name":"x","labels":{"a":"b"}}}`,
			overlay: `{"metadata":{"labels":{"$patch":"delete"}}}`,
			want:    `{"metadata":{"name":"x"}}`,
		},
		{
			// Non-name merge keys are not modelled, so the list is replaced,
			// but the delete marker must not be stored.
			name:    "non-name list drops patch markers",
			base:    `{"spec":{"containers":[{"name":"web","ports":[{"containerPort":80},{"containerPort":443}]}]}}`,
			overlay: `{"spec":{"containers":[{"name":"web","ports":[{"$patch":"delete","containerPort":443}]}]}}`,
			want:    `{"spec":{"containers":[{"name":"web","ports":[]}]}}`,
		},
		{
			name:    "unknown directive is dropped",
			base:    `{"metadata":{"name":"x"}}`,
			overlay: `{"metadata":{"$unknown":"y","namespace":"default"}}`,
			want:    `{"metadata":{"name":"x","namespace":"default"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := decodeJSONMap(t, tc.base)
			overlay := decodeJSONMap(t, tc.overlay)
			want := decodeJSONMap(t, tc.want)
			jsonMerge(base, overlay)
			if !reflect.DeepEqual(base, want) {
				gotJSON, _ := json.MarshalIndent(base, "", "  ")
				wantJSON, _ := json.MarshalIndent(want, "", "  ")
				t.Fatalf("merged object mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
			}
			assertNoDirectiveKeys(t, base)
		})
	}
}
