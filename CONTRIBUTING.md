# Contributing

## Layout

| File | Role |
| --- | --- |
| `main.go` | start order, logging, signals, HTTP server settings |
| `config.go` | flags and `JUSTSERVE_*` variables, validation |
| `site.go` | the startup snapshot: published files, encodings, caching policy |
| `handler.go` | per-request decisions and response headers |
| `mime.go` | content-type table |
| `charts/justserve` | Helm chart |

Tests live next to the code:
- `handler_test.go` and `config_test.go` cover the behaviour.
- `server_test.go` uses real TCP connections.
- `main_test.go` re-executes the test binary as the server, to check log records,
  signals and exit status.

```sh
go test -race ./...
go run . -root ./dist -fallback /index.html -immutable /assets/
```

## Design rules

- **Request data never reaches the filesystem.** The request path is only a key into
  the snapshot built at startup, and responses are read from descriptors opened then.
- **The listener opens only after the whole root has loaded.** A bad configuration or an
  unsafe entry stops startup; nothing is served from a partial root.
- **Headers the server sets are not configurable.** `-header` rejects `Cache-Control`,
  `Content-Type`, `X-Content-Type-Options` and the like.
- **No write timeout.** It would cut large downloads to slow clients; the gateway limits
  connections instead.
- **The standard library only.**

## Edge cases

- **`OPTIONS *`**: `net/http` answers it itself unless `DisableGeneralOptionsHandler`
  is set. Keep that option set, or the request bypasses the handler and its headers.
- **Content-Length on encoded responses**: `ServeContent` leaves it out of encoded 200
  responses. `fullLength` adds it for status 200 only. Setting it earlier breaks 412
  responses, which would declare a body they don't send.
- **Environment names**: no variable name may be a prefix of another's labelled form
  (`TestEnvironmentNamesAreUnambiguous`). For example, a `-header-timeout` flag would
  collide with `JUSTSERVE_HEADER_*`.

## Environment

- **Reproducible binaries**: these come only from the `Containerfile`, whose toolchain
  is pinned. Distribution builds of Go differ; Fedora's sets `GOEXPERIMENT=nodwarf5`.
- **Local multi-arch manifests**: `podman create --platform` picks the host
  architecture. Inspect the images by their manifest digest instead.
- **kind**:
  - kube-proxy needs a higher inotify limit:
    `sudo sysctl -w fs.inotify.max_user_instances=512`;
  - kindnet drops replies when a NetworkPolicy denies egress, which is why
    `networkPolicy.denyEgress` is off by default.

## Testing the chart end to end

Use a kind cluster with its own kubeconfig:
1. Install Envoy Gateway with its Helm chart
   (`oci://docker.io/envoyproxy/gateway-helm`).
2. Create a Gateway `edge` in `envoy-gateway-system`.
3. Install this chart with `httpRoute.parentRefs` pointing at `edge`.
4. Send requests continuously to the Envoy Service while running `helm upgrade`,
   `kubectl rollout restart` and scaling. No request may fail.

## Releasing

Push a tag `v<major>.<minor>.<patch>`; a suffix such as `-rc1` makes a prerelease.
`release.yml` reruns CI, then publishes to ghcr.io:
- the image, with an SBOM and provenance;
- the Helm chart;
- the binaries, with `SHA256SUMS`.

Everything is signed keyless with the workflow's identity.

New ghcr.io packages start out private. After the first release, make `justserve` and
`charts/justserve` public.

GitHub Actions are pinned by commit and kept current by Dependabot. The builder image in
the `Containerfile` is updated by hand: change its tag and its index digest together.
