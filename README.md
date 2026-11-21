# justserve

A static file server for Kubernetes: serves single-page apps and other static sites, as a
replacement for nginx. Shell-less container image, go standard library only, ~700 lines.

## Serving a site

Build a site image on top of justserve:

```Dockerfile
FROM ghcr.io/belohnung/justserve@sha256:<digest>
COPY dist /srv
ENV JUSTSERVE_FALLBACK=/index.html \
    JUSTSERVE_IMMUTABLE_ASSETS=/assets/
```

The site is read once at startup. To publish new content, roll out a new image; files
changed under a running server are not served.

To deploy on Kubernetes, use the [Helm chart](charts/justserve/README.md).

RAM caching is left to the kernel: file contents live in the page cache, which is
charged to the container's memory limit and evicted under pressure instead of getting
the pod OOM-killed. Only the gzip copies made at startup stay in the server's own memory.

## Responses

| Request | Response |
| --- | --- |
| an existing file | the file, in the smallest encoding the client accepts |
| `/dir/` | `/dir/index.html` |
| `/dir`, when `/dir/index.html` exists | redirect to `/dir/` |
| a missing path, with `-fallback` set | the fallback document with 200 (SPA routing) |
| a missing path under an `-immutable` prefix | 404, never the fallback |
| a missing path, with `-not-found` set | that document with 404 |
| a method other than GET or HEAD | 405 |

- **Caching**:
  - files under `-immutable` prefixes are cached for a year;
  - everything else gets `-cache-control` (default `no-cache`);
  - ETags are content hashes, so they are the same on every replica and survive
    rebuilds.
- **Compression**:
  - text-like files and WebAssembly are gzipped at startup;
  - with `-precompressed`, `.br`, `.zst` and `.gz` files next to a file are served as
    its encodings instead.
- **Content types**:
  - they are fixed per extension; add or change one with `-type`;
  - files with an unknown extension or none get `-default-type`
    (`application/octet-stream` unless set);
  - browsers are always told not to guess another type (`nosniff`).
- **Files**:
  - every file under the root is served, dotfiles included;
  - symlinks may not point outside the root;
  - ConfigMap and Secret volumes mounted under the root work.

## Configuration

Every flag can also be set as an environment variable, so a site image sets its
defaults with `ENV` and a Deployment overrides single keys.

| Flag | Variable | Default | |
| --- | --- | --- | --- |
| `-root DIR` | `JUSTSERVE_ROOT` | `/srv` | directory to serve |
| `-listen ADDR` | `JUSTSERVE_LISTEN` | `:8080` | listen address |
| `-fallback PATH` | `JUSTSERVE_FALLBACK` | | served with 200 for missing paths, e.g. `/index.html` |
| `-not-found PATH` | `JUSTSERVE_NOT_FOUND` | | served with 404 for missing paths, e.g. `/404.html` |
| `-immutable PREFIX` | `JUSTSERVE_IMMUTABLE_<label>` | | content-hashed URL prefix such as `/assets/` |
| `-cache-control VALUE` | `JUSTSERVE_CACHE_CONTROL` | `no-cache` | Cache-Control for everything else |
| `-header 'Name: value'` | `JUSTSERVE_HEADER_<label>` | | sent with every response |
| `-type .ext=media/type` | `JUSTSERVE_TYPE_<label>` | | add or override a content type |
| `-default-type TYPE` | `JUSTSERVE_DEFAULT_TYPE` | `application/octet-stream` | content type for files without a known extension, like nginx's `default_type` |
| `-precompressed` | `JUSTSERVE_PRECOMPRESSED=true` | `false` | use `.br` / `.zst` / `.gz` files instead of compressing at startup |
| `-read-timeout D` | `JUSTSERVE_READ_TIMEOUT` | `10s` | close connections that haven't sent a complete request within D |
| `-idle-timeout D` | `JUSTSERVE_IDLE_TIMEOUT` | `2h` | close idle connections; keep it above the gateway's upstream idle timeout |

- **Repeatable flags** take one variable per value, with a free-form label such as
  `JUSTSERVE_HEADER_CSP`. Values apply in label order.
- **Precedence**: the environment applies first. Single-valued flags on the command line
  replace it; repeatable ones add to it.
- **Empty variables count as unset**: `JUSTSERVE_HEADER_CSP=` in a Deployment removes a
  header the image sets.
- **Invalid configuration** stops the server at startup, with a log message naming the
  flag or variable.

Logs go to stderr in logfmt. The `config` record at startup shows the version and the
effective configuration.

## Verifying a release

Images are published for linux/amd64 and linux/arm64 to `ghcr.io/belohnung/justserve`
and are signed. Each release's notes list its digests. To verify one:

```sh
cosign verify ghcr.io/belohnung/justserve@sha256:<digest> \
  --certificate-identity https://github.com/belohnung/justserve/.github/workflows/release.yml@refs/tags/<tag> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Migrating from nginx

A typical SPA configuration has three parts:
- a `try_files` fallback to `index.html`;
- long-lived caching for hashed assets;
- security headers.

These become:

```Dockerfile
ENV JUSTSERVE_FALLBACK=/index.html \
    JUSTSERVE_IMMUTABLE_ASSETS=/assets/ \
    JUSTSERVE_HEADER_REFERRER="Referrer-Policy: no-referrer" \
    JUSTSERVE_HEADER_CSP="Content-Security-Policy: frame-ancestors 'none'"
```

Differences to expect:
- **No allowlist**: existing files are always served as files, so nginx rules that
  pass through files like `config.json` or `sw.js` are not needed.
- **Missing files fall back**: outside the immutable prefixes, a missing file gets the
  fallback document rather than a 404.
- **No writable volume**: nothing is written, so the `/tmp` volume that unprivileged
  nginx needs can go.

## Not included

TLS, HTTP/2, access logs and metrics are left to the gateway in front. There is no live
reload.

## License

Apache-2.0; see [LICENSE](LICENSE).
