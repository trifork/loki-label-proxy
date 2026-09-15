// Package proxy serves the tenant-facing side of loki-label-proxy: it resolves
// the label value for each request, rewrites the query, and forwards upstream.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/trifork/loki-label-proxy/internal/enforce"
)

// labelValuePattern is deliberately stricter than Loki's own rules. The value
// arrives in a header, so keeping it to the shape of a Kubernetes name leaves
// no room for quoting tricks against the LogQL serialiser.
var labelValuePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Config describes a proxy instance.
type Config struct {
	Upstream   *url.URL
	Enforcer   *enforce.Enforcer
	HeaderName string
	Logger     *slog.Logger
	// Registerer receives the proxy's own metrics. Defaults to a throwaway
	// registry so tests and embedders need not supply one.
	Registerer prometheus.Registerer
}

// Handler is the tenant-facing HTTP handler.
type Handler struct {
	cfg     Config
	proxy   *httputil.ReverseProxy
	mux     *http.ServeMux
	metrics *metrics
}

// New builds a Handler. Every route is registered explicitly; anything not
// named here returns 404 rather than reaching Loki.
func New(cfg Config) (*Handler, error) {
	if cfg.Upstream == nil {
		return nil, errors.New("proxy: upstream must not be nil")
	}
	if cfg.Enforcer == nil {
		return nil, errors.New("proxy: enforcer must not be nil")
	}
	if cfg.HeaderName == "" {
		return nil, errors.New("proxy: header name must not be empty")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Registerer == nil {
		cfg.Registerer = prometheus.NewRegistry()
	}

	h := &Handler{cfg: cfg, mux: http.NewServeMux(), metrics: newMetrics(cfg.Registerer)}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(cfg.Upstream)
			r.Out.Host = cfg.Upstream.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			cfg.Logger.Error("upstream request failed", "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadGateway, "upstream request failed")
		},
	}

	// Query endpoints: the parameter holds a full LogQL expression.
	for _, path := range []string{
		"/loki/api/v1/query",
		"/loki/api/v1/query_range",
		"/loki/api/v1/index/stats",
		"/loki/api/v1/index/volume",
		"/loki/api/v1/index/volume_range",
		"/loki/api/v1/patterns",
		"/loki/api/v1/detected_labels",
		"/loki/api/v1/detected_fields",
	} {
		h.mux.HandleFunc(path, h.handleQuery("query"))
	}

	// Metadata endpoints: the query parameter is optional. When absent we
	// inject a bare selector, so an unscoped call cannot enumerate everything.
	h.mux.HandleFunc("/loki/api/v1/labels", h.handleQuery("query"))
	h.mux.HandleFunc("/loki/api/v1/label", h.handleQuery("query"))
	h.mux.HandleFunc("/loki/api/v1/label/", h.handleLabelValues())

	// The series endpoint takes bare selectors, possibly several.
	h.mux.HandleFunc("/loki/api/v1/series", h.handleSeries())

	// Deny by default. Registered explicitly rather than left to ServeMux's
	// fallback, so unknown paths get the same JSON error shape as everything
	// else and the intent is visible here.
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})

	// Carry no log data, so they need no enforcement.
	for _, path := range []string{
		"/loki/api/v1/format_query",
		"/loki/api/v1/status/buildinfo",
		"/api/v1/status/buildinfo",
	} {
		h.mux.HandleFunc(path, h.handlePassthrough())
	}

	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	h.mux.ServeHTTP(rec, r)
	// Pattern, not r.URL.Path: the raw path of /label/<name>/values would put
	// every label name into the metric's cardinality.
	pattern := "other"
	if _, p := h.mux.Handler(r); p != "" {
		pattern = p
	}
	h.metrics.observe(pattern, rec.code)
}

// value resolves the enforced label value for a request, or writes the error
// response and reports false. A missing or malformed header is fatal to the
// request: this proxy has no notion of an unrestricted caller, and a caller
// that should see everything belongs upstream of it, not through it.
func (h *Handler) value(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.Header.Get(h.cfg.HeaderName)
	if v == "" {
		h.metrics.reject(reasonMissingHeader)
		h.cfg.Logger.Warn("request without label header", "path", r.URL.Path, "header", h.cfg.HeaderName)
		writeError(w, http.StatusUnauthorized, fmt.Sprintf("missing %s header", h.cfg.HeaderName))
		return "", false
	}
	if !labelValuePattern.MatchString(v) {
		h.metrics.reject(reasonMalformedHeader)
		h.cfg.Logger.Warn("request with malformed label header", "path", r.URL.Path, "header", h.cfg.HeaderName)
		writeError(w, http.StatusBadRequest, fmt.Sprintf("malformed %s header", h.cfg.HeaderName))
		return "", false
	}
	return v, true
}

// handleQuery enforces a full LogQL expression held in the named parameter.
// An absent or empty parameter yields the bare enforced selector.
//
// Note this proxy does not police whether a query was required. Loki's own API
// requires one on most of these endpoints, but validating that here only makes
// the proxy stricter than the thing it fronts -- and Grafana issues plenty of
// speculative calls with no query yet. Injecting the selector keeps the request
// enforced, which is the only thing this process is responsible for, and lets
// Loki answer for its own contract.
func (h *Handler) handleQuery(param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, ok := h.value(w, r)
		if !ok {
			return
		}
		h.rewrite(w, r, param, func(in string, present bool) (string, error) {
			if !present {
				return h.cfg.Enforcer.Selector(value)
			}
			return h.cfg.Enforcer.Query(in, value)
		})
	}
}

// handleSeries enforces every match[] selector, injecting one when the caller
// supplied none.
func (h *Handler) handleSeries() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, ok := h.value(w, r)
		if !ok {
			return
		}
		h.rewriteAll(w, r, "match[]", func(in []string) ([]string, error) {
			if len(in) == 0 {
				sel, err := h.cfg.Enforcer.Selector(value)
				if err != nil {
					return nil, err
				}
				return []string{sel}, nil
			}
			out := make([]string, 0, len(in))
			for _, sel := range in {
				// Same reasoning as above: an empty match[] is a request for
				// everything, not a malformed selector.
				if strings.TrimSpace(sel) == "" {
					continue
				}
				rewritten, err := h.cfg.Enforcer.Matchers(sel, value)
				if err != nil {
					return nil, err
				}
				out = append(out, rewritten)
			}
			if len(out) == 0 {
				sel, err := h.cfg.Enforcer.Selector(value)
				if err != nil {
					return nil, err
				}
				return []string{sel}, nil
			}
			return out, nil
		})
	}
}

// handleLabelValues serves /loki/api/v1/label/<name>/values. Asking for the
// values of the enforced label itself is answered directly: the caller is
// entitled to exactly one, and going upstream for it would return every
// tenant's.
func (h *Handler) handleLabelValues() http.HandlerFunc {
	inner := h.handleQuery("query")
	return func(w http.ResponseWriter, r *http.Request) {
		value, ok := h.value(w, r)
		if !ok {
			return
		}
		name, isValues := labelNameFromPath(r.URL.Path)
		if !isValues {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if name == h.cfg.Enforcer.Label() {
			writeJSON(w, fmt.Sprintf(`{"status":"success","data":[%q]}`, value))
			return
		}
		inner(w, r)
	}
}

func (h *Handler) handlePassthrough() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.value(w, r); !ok {
			return
		}
		h.forward(w, r)
	}
}

// rewrite applies fn to a single-valued parameter.
func (h *Handler) rewrite(w http.ResponseWriter, r *http.Request, param string, fn func(string, bool) (string, error)) {
	h.rewriteAll(w, r, param, func(in []string) ([]string, error) {
		var cur string
		if len(in) > 0 {
			cur = strings.TrimSpace(in[0])
		}
		// Present-but-empty counts as absent. Grafana sends a bare "query="
		// on its metadata calls, and handing that to the parser yields
		// "unexpected $end" rather than the unscoped-request handling the
		// caller actually meant.
		present := cur != ""
		out, err := fn(cur, present)
		if err != nil {
			return nil, err
		}
		return []string{out}, nil
	})
}

// rewriteAll rewrites a parameter in place, in the query string for GET and in
// the form body for POST. Grafana switches to POST once a query grows past a
// certain length, so both paths have to work; handling only the query string is
// the classic "fine in Explore, broken in a dashboard" bug.
func (h *Handler) rewriteAll(w http.ResponseWriter, r *http.Request, param string, fn func([]string) ([]string, error)) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		out, err := fn(q[param])
		if err != nil {
			h.writeRewriteError(w, r, err)
			return
		}
		q[param] = out
		r.URL.RawQuery = q.Encode()

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		// ParseForm merges the query string into r.Form; rewrite there and
		// send everything as the body so no un-enforced copy survives in the
		// URL.
		out, err := fn(r.Form[param])
		if err != nil {
			h.writeRewriteError(w, r, err)
			return
		}
		r.Form[param] = out
		body := r.Form.Encode()
		r.URL.RawQuery = ""
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	h.forward(w, r)
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request) {
	// The datasource supplies the tenant header; a second one from the client
	// must never reach Loki.
	r.Header.Del(h.cfg.HeaderName)
	h.proxy.ServeHTTP(w, r)
}

func (h *Handler) writeRewriteError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, enforce.ErrLabelPresent) {
		h.metrics.reject(reasonLabelPresent)
		h.cfg.Logger.Warn("rejected query constraining the enforced label",
			"path", r.URL.Path, "label", h.cfg.Enforcer.Label())
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("query must not contain a matcher for %q", h.cfg.Enforcer.Label()))
		return
	}
	h.metrics.reject(reasonUnparseable)
	h.cfg.Logger.Debug("rejected unparseable query", "path", r.URL.Path, "error", err)
	writeError(w, http.StatusBadRequest, err.Error())
}

// labelNameFromPath extracts <name> from /loki/api/v1/label/<name>/values.
func labelNameFromPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/loki/api/v1/label/")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, "/values")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"status":"error","error":%q}`, msg)
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}
