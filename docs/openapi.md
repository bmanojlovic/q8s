# OpenAPI schema and embedded CRDs

q8s embeds two generated files in the server binary:

| File | Contents |
|---|---|
| `internal/server/openapi_v2.json` | Trimmed Swagger 2.0 document served at `/openapi/v2` |
| `internal/server/crds.json` | Read-only `CustomResourceDefinitionList` served at `/apis/apiextensions.k8s.io/v1/customresourcedefinitions` |

Both are outputs; the upstream files are not committed. Regenerate them with
`hack/gen-openapi`, which needs the upstream Kubernetes `swagger.json` that
matches `k8s.io/api` and the Traefik CRD YAML.

## Regenerate

From the repository root:

```sh
go run ./hack/gen-openapi \
    -swagger /path/to/k8s-v1.36.2-swagger.json \
    -crds /path/to/traefik-v3.6-crds.yml
```

The default output paths are `internal/server/openapi_v2.json` and
`internal/server/crds.json`. Pass `-openapi-out` or `-crds-out` to write
elsewhere.

The upstream Kubernetes swagger lives at
`https://github.com/kubernetes/kubernetes/blob/<tag>/api/openapi-spec/swagger.json`.
The Traefik CRD file is published with each Traefik release as `crds.yaml`.

## What is kept

`hack/gen-openapi/main.go` has one list, `servedGVKs`, naming every resource
kind q8s serves. The generator keeps:

- the definition for each served kind and its `...List` kind
- `meta.v1.Status`, `meta.v1.DeleteOptions` and `meta.v1.Patch`
- the transitive closure of every `$ref` those definitions use

Everything else from the upstream swagger is dropped. The output is compact
and deterministic (JSON map keys are sorted), so regenerating without input
changes produces no diff. The generator fails on a dangling `$ref`.

When adding or removing a served resource, update both the discovery
handlers in `internal/server/discovery.go` and `servedGVKs` in the
generator, then regenerate.

The CRD list keeps only `middlewares.traefik.io` and
`ingressroutetcps.traefik.io` from the Traefik file. Update `wantedCRDs` in
the generator when the served CRD names change.

## Bumping versions

When bumping `k8s.io/api` in `go.mod`:

1. Download the `swagger.json` from the Kubernetes tag matching the new
   module version (`k8s.io/api v0.36.2` maps to Kubernetes v1.36.2).
2. Update the `openapiVersion` constant in `hack/gen-openapi/main.go`.
3. Run the generator with the new swagger.
4. Run `go test ./...`; the OpenAPI test checks that every kind advertised
   by discovery has a definition with a matching
   `x-kubernetes-group-version-kind`.

When bumping Traefik, download the new `crds.yaml`, adjust `wantedCRDs` if
the names changed, and run the generator with `-crds`.

The trimmed document has real definitions, so kubectl client-side
validation now enforces the upstream schemas instead of accepting any
object shape. The protobuf representation of the same document is generated
lazily from the embedded JSON for clients that request
`application/com.github.proto-openapi.spec.v2.v1.0+protobuf`.
