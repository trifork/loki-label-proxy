# loki-label-proxy

A reverse proxy that enforces a label matcher on every LogQL query it forwards
to [Grafana Loki](https://github.com/grafana/loki). Callers can query their own
streams and nothing else, whatever they send.

It is the Loki counterpart to
[prom-label-proxy](https://github.com/prometheus-community/prom-label-proxy),
and takes the same view of its job: **it performs no authentication and no
authorization.** Whatever sits in front of it decides who the caller is and sets
the label value header accordingly. This process only guarantees that the value
is honoured — that the caller cannot see streams outside it, widen the scope, or
enumerate around it.

## Run it

Images are published to GitHub Container Registry on every release, tagged with
the full version and with `major.minor`:

```
docker run --rm -p 8080:8080 -p 8081:8081 \
  ghcr.io/trifork/loki-label-proxy:1.0.0 \
  -upstream=http://loki-read.observability.svc.cluster.local:3100 \
  -label=tenant_namespace \
  -header-name=X-Tenant-Namespace
```

| flag | default | meaning |
|---|---|---|
| `-upstream` | *(required)* | Loki read endpoint to forward to. Must be an absolute URL |
| `-label` | *(required)* | label name to enforce on every query |
| `-header-name` | *(required)* | request header carrying the enforced label value |
| `-error-on-replace` | `true` | reject queries that already constrain the enforced label, instead of silently replacing the matcher |
| `-insecure-listen-address` | `:8080` | caller-facing address |
| `-internal-listen-address` | `:8081` | `/metrics` and `/healthz`, kept off the caller-facing port |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

`-error-on-replace` defaults to rejecting because silently replacing a
deliberate cross-scope query with an innocuous one hides the attempt. Failing
loudly surfaces it.

The label value arrives in a header, so it is held to a deliberately narrow
shape — `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, the shape of a Kubernetes name. This
leaves no room for quoting tricks against the LogQL serialiser, but it does mean
a value with uppercase letters, dots or underscores is rejected as malformed
rather than forwarded.

Any copy of the header sent by the client is deleted before the request goes
upstream, so a caller cannot smuggle a second value past the one set for them.

### On Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: loki-label-proxy
spec:
  replicas: 2
  selector:
    matchLabels: {app: loki-label-proxy}
  template:
    metadata:
      labels: {app: loki-label-proxy}
    spec:
      containers:
        - name: proxy
          image: ghcr.io/trifork/loki-label-proxy:1.0.0
          args:
            - -upstream=http://loki-read.observability.svc.cluster.local:3100
            - -label=tenant_namespace
            - -header-name=X-Tenant-Namespace
          ports:
            - {name: proxy, containerPort: 8080}
            - {name: internal, containerPort: 8081}
          readinessProbe:
            httpGet: {path: /healthz, port: internal}
```

The image runs as `nonroot` with a read-only-friendly static binary and no
shell, so it needs no privileges and no writable filesystem.

## Point Grafana at it

Configure a Loki datasource whose URL is the proxy rather than Loki, and have it
send the label value header:

```yaml
apiVersion: 1
datasources:
  - name: Loki (team-a)
    type: loki
    url: http://loki-label-proxy.observability.svc.cluster.local:8080
    jsonData:
      httpHeaderName1: X-Tenant-Namespace
    secureJsonData:
      httpHeaderValue1: team-a
```

Note what this does and does not buy you. A datasource with a fixed header value
scopes *that datasource*, which is the right answer when each team has its own
Grafana organisation or its own datasource. It is not access control: anyone who
can reach the proxy directly can send their own header. The component that
decides which value a caller is entitled to — an authenticating proxy, a
gateway, Grafana itself — belongs in front of this one.

## What it enforces

**Deny by default.** Only the endpoints below are served; anything else returns
404 rather than reaching Loki.

**Fails closed.** Every served endpoint requires a valid value header, including
those that forward unchanged. There is no notion of an unrestricted caller:
anyone who should see everything belongs upstream of this proxy, not through it.

| endpoint | enforcement |
|---|---|
| `/loki/api/v1/query`, `/query_range` | `query` rewritten |
| `/loki/api/v1/index/stats`, `/index/volume`, `/index/volume_range` | `query` rewritten |
| `/loki/api/v1/patterns`, `/detected_labels`, `/detected_fields` | `query` rewritten |
| `/loki/api/v1/labels`, `/label` | `query` rewritten |
| `/loki/api/v1/label/<name>/values` | `query` rewritten; asking for the enforced label's own values returns exactly the caller's value, without going upstream |
| `/loki/api/v1/series` | every `match[]` rewritten; one injected if the caller sent none |
| `/loki/api/v1/format_query`, `/loki/api/v1/status/buildinfo`, `/api/v1/status/buildinfo` | forwarded unchanged; they carry no log data |
| anything else | `404` |

Enforcing the metadata endpoints matters as much as the query ones. Left alone,
`/labels` and `/label/<name>/values` happily enumerate every value in the Loki
tenant, including every other caller's.

**An absent or empty parameter yields the bare enforced selector** —
`{tenant_namespace="team-a"}` — rather than an error. Loki's own API does require
a query on most of these endpoints, but enforcing that here would make the proxy
stricter than the thing it fronts, and Grafana issues plenty of speculative
calls with no query yet. The request goes upstream scoped, which is this
process's only responsibility, and Loki answers for its own contract. The same
applies to a `match[]` that is present but empty.

Both `GET` query strings and `POST` form bodies are rewritten. Grafana switches
to `POST` once a query grows past a certain length, so handling only the query
string produces a proxy that works in Explore and leaks in a dashboard. On
`POST` the rewritten parameters are sent as the body and the query string is
cleared, so no un-enforced copy survives in the URL.

## Responses

| code | when |
|---|---|
| `401` | no value header |
| `400` | malformed value header, unparseable query, or — with `-error-on-replace` — a query already constraining the enforced label |
| `404` | unrecognised path |
| `405` | anything other than `GET` or `POST` on an enforced endpoint |
| `502` | upstream Loki unreachable or failed |

Errors are returned as JSON — `{"status":"error","error":"..."}` — so a Grafana
datasource surfaces the message rather than a bare status code.

## Metrics

Served on the internal listener, never the caller-facing one, alongside
`/healthz`.

| metric | labels | meaning |
|---|---|---|
| `loki_label_proxy_requests_total` | `path`, `code` | requests handled. `path` is the matched route, not the raw URL, so `/label/<name>/values` stays one series |
| `loki_label_proxy_rejected_total` | `reason` | requests refused before reaching Loki: `missing_header`, `malformed_header`, `label_present`, `unparseable_query` |

`rejected_total` is the one worth alerting on. A rising `label_present` rate is
a caller probing the boundary; a rising `missing_header` rate usually means a
datasource has lost its header configuration.

## How it works

Rewriting is done against Loki's own LogQL grammar (`logql/syntax`) rather than
by pattern matching on the query text, because a stream selector can appear in
more places than are obvious — inside a range aggregation, once on each side of
a binary operation — and because `line_format "{{.a}}"` contains braces that are
not a selector at all. Parsing the query and walking the AST handles all of
them.

## Building

```
make build          # static binary in bin/
make test           # go test -race -cover ./...
make vet fmt tidy
make docker         # local image, override IMAGE and TAG
```

## License

AGPL-3.0. This program links Loki's LogQL parser, which is itself AGPL-3.0;
running it as a network service therefore carries the obligation in section 13
to offer users its source. Publishing the source here is how that obligation is
met.
