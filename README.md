# loki-label-proxy

A reverse proxy that enforces a label matcher on every LogQL query it forwards
to [Grafana Loki](https://github.com/grafana/loki).

It is the Loki counterpart to
[prom-label-proxy](https://github.com/prometheus-community/prom-label-proxy),
and takes the same view of its job: **it performs no authentication and no
authorization.** Whatever sits in front of it decides who the caller is and sets
the label value header accordingly. This process only guarantees that the value
is honoured — that the caller cannot see streams outside it, widen the scope, or
enumerate around it.

## Why not rewrite the query with a regular expression

Because a LogQL query is not a regular language, and the places a stream
selector can hide are not obvious:

```logql
sum by (pod) (rate({app="x"}[5m]))        # inside a range aggregation
sum(rate({app="x"}[5m])) / sum(rate({app="y"}[5m]))   # once per side
{app="x"} | line_format "{{.a}}"          # those braces are NOT a selector
```

The last one is the trap. A rewriter looking for `{...}` sees the `{{ }}` of a
Go template and concludes the query has two stream selectors. This proxy parses
with Loki's own grammar (`logql/syntax`) and walks the resulting AST, so all
three cases are handled for free, along with every future one.

## Usage

```
loki-label-proxy \
  -upstream=http://loki-read.observability.svc.cluster.local:3100 \
  -label=tenant_namespace \
  -header-name=X-Tenant-Namespace
```

| flag | default | meaning |
|---|---|---|
| `-upstream` | *(required)* | Loki read endpoint to forward to |
| `-label` | *(required)* | label name to enforce on every query |
| `-header-name` | *(required)* | request header carrying the enforced label value |
| `-error-on-replace` | `true` | reject queries that already constrain the enforced label, instead of silently replacing the matcher |
| `-insecure-listen-address` | `:8080` | caller-facing address |
| `-internal-listen-address` | `:8081` | `/metrics` and `/healthz`, kept off the caller-facing port |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

`-error-on-replace` defaults to rejecting because silently replacing a
deliberate cross-scope query with an innocuous one hides the attempt. Failing
loudly surfaces it.

## Behaviour

**Fails closed.** A request with no value header, or a malformed one, is
rejected before routing. There is no notion of an unrestricted caller: anyone
who should see everything belongs upstream of this proxy, not through it.

**Deny by default.** Only the endpoints below are served; anything else returns
404 rather than reaching Loki.

| endpoint | enforcement |
|---|---|
| `/loki/api/v1/query`, `/query_range` | `query` rewritten |
| `/loki/api/v1/index/stats`, `/index/volume`, `/index/volume_range` | `query` rewritten |
| `/loki/api/v1/patterns`, `/detected_labels`, `/detected_fields` | `query` rewritten |
| `/loki/api/v1/series` | every `match[]` rewritten; one injected if absent |
| `/loki/api/v1/labels`, `/label` | `query` rewritten, or a bare selector injected |
| `/loki/api/v1/label/<name>/values` | as above; asking for the enforced label's own values returns exactly the caller's value, without going upstream |
| `/loki/api/v1/format_query`, `/status/buildinfo` | forwarded unchanged (carry no log data) |
| anything else | `404` |

Enforcing the metadata endpoints matters as much as the query ones. Left alone,
`/labels` and `/label/<name>/values` happily enumerate every value in the Loki
tenant, including every other caller's.

Both `GET` query strings and `POST` form bodies are rewritten. Grafana switches
to `POST` once a query grows past a certain length, so handling only the query
string produces a proxy that works in Explore and leaks in a dashboard.

## Metrics

Served on the internal listener, never the caller-facing one.

| metric | labels | meaning |
|---|---|---|
| `loki_label_proxy_requests_total` | `path`, `code` | requests handled. `path` is the matched route, not the raw URL, so `/label/<name>/values` stays one series |
| `loki_label_proxy_rejected_total` | `reason` | requests refused before reaching Loki: `missing_header`, `malformed_header`, `label_present`, `unparseable_query` |

`rejected_total` is the one worth alerting on. A rising `label_present` rate is
a caller probing the boundary; a rising `missing_header` rate usually means a
datasource has lost its header configuration.

## Building

```
make build
make test
```

## License

AGPL-3.0. This program links Loki's LogQL parser, which is itself AGPL-3.0;
running it as a network service therefore carries the obligation in section 13
to offer users its source. Publishing the source here is how that obligation is
met.
