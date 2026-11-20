# justserve Helm chart

Deploys a site image built on justserve:
- it meets the restricted Pod Security Standard;
- it runs 2 replicas, with a disruption budget and a `preStop` delay so rollouts drop no
  requests;
- a NetworkPolicy admits only Envoy Gateway's namespace;
- a Gateway API `HTTPRoute` is optional.

## Install

Build the site image first (see the [justserve README](../../README.md#serving-a-site)),
then install with your values:

```yaml
# values.yaml
image:
  reference: registry.example.org/site@sha256:<digest>
env:
  JUSTSERVE_HEADER_CSP: "Content-Security-Policy: frame-ancestors 'none'"
httpRoute:
  enabled: true
  parentRefs:
    - name: edge
      namespace: envoy-gateway-system
  hostnames: [site.example.org]
```

```sh
helm install site oci://ghcr.io/belohnung/charts/justserve --version <version> -f values.yaml
```

To verify the chart first, use the digest from the release notes:

```sh
cosign verify ghcr.io/belohnung/charts/justserve@sha256:<digest> \
  --certificate-identity https://github.com/belohnung/justserve/.github/workflows/release.yml@refs/tags/<tag> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Values

| Value | Default | |
| --- | --- | --- |
| `image.reference` | required | the site image; use a digest |
| `image.pullPolicy` | `IfNotPresent` | |
| `replicaCount` | `2` | |
| `env` | `{}` | `JUSTSERVE_*` settings, applied per key over the image's `ENV`; an empty value unsets one |
| `containerPort` / `service.port` | `8080` / `80` | |
| `resources` | requests 50m / 64Mi, limits 250m / 128Mi | memory beyond the server's own use caches the site |
| `preStopSleepSeconds` | `5` | how long a terminating pod keeps serving |
| `startupTimeoutSeconds` | `120` | how long loading the site may take |
| `podDisruptionBudget.enabled` / `.maxUnavailable` | `true` / `1` | |
| `networkPolicy.enabled` | `true` | |
| `networkPolicy.from` | namespace `envoy-gateway-system` | who may reach the pods; must not be empty |
| `networkPolicy.denyEgress` | `false` | also block all outbound traffic; see below |
| `httpRoute.enabled` / `.parentRefs` / `.hostnames` | `false` / — / — | `parentRefs` is required when enabled |
| `imagePullSecrets`, `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | passed through |

Invalid values are rejected at install time.

**`denyEgress`**: justserve never connects anywhere, so blocking outbound traffic is
safe in principle. Some network plugins, however, also drop the replies to allowed
requests. Enable it only after checking that the site still answers through the gateway.
