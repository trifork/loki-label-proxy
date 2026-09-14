package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/trifork/loki-label-proxy/internal/enforce"
)

func newMeteredHandler(t *testing.T) (*Handler, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parsing upstream: %v", err)
	}
	enforcer, err := enforce.New(label, true)
	if err != nil {
		t.Fatalf("enforce.New: %v", err)
	}
	h, err := New(Config{
		Upstream:   target,
		Enforcer:   enforcer,
		HeaderName: headerName,
		Registerer: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, reg
}

func TestRejectionsAreCounted(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		headerValue string
		reason      string
	}{
		{"missing header", `/loki/api/v1/query_range?query={app%3D"x"}`, "", reasonMissingHeader},
		{"malformed header", `/loki/api/v1/query_range?query={app%3D"x"}`, "Bad Value", reasonMalformedHeader},
		{"caller-supplied matcher", `/loki/api/v1/query_range?query={tenant_namespace%3D"other"}`, value, reasonLabelPresent},
		{"unparseable query", `/loki/api/v1/query_range?query={app%3D"x"`, value, reasonUnparseable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, reg := newMeteredHandler(t)
			do(t, h, http.MethodGet, tt.target, "", tt.headerValue)

			got := testutil.ToFloat64(h.metrics.rejected.WithLabelValues(tt.reason))
			if got != 1 {
				t.Errorf("rejected_total{reason=%q} = %v, want 1", tt.reason, got)
			}
			if n := testutil.CollectAndCount(reg, "loki_label_proxy_rejected_total"); n != 1 {
				t.Errorf("counted %d rejection series, want exactly 1", n)
			}
		})
	}
}

// The raw path of /label/<name>/values would otherwise put every label name a
// caller asks about into the metric's cardinality.
func TestRequestMetricUsesRoutePatternNotRawPath(t *testing.T) {
	h, _ := newMeteredHandler(t)

	for _, name := range []string{"app", "pod", "container"} {
		do(t, h, http.MethodGet, "/loki/api/v1/label/"+name+"/values", "", value)
	}

	got := testutil.ToFloat64(h.metrics.requests.WithLabelValues("/loki/api/v1/label/", "502"))
	if got != 3 {
		t.Errorf("requests_total for the label-values route = %v, want 3 under a single series", got)
	}
}

func TestUnknownPathIsCountedWithoutLeakingCardinality(t *testing.T) {
	h, _ := newMeteredHandler(t)

	for _, p := range []string{"/nope", "/also-nope", "/definitely/not"} {
		do(t, h, http.MethodGet, p, "", value)
	}

	if got := testutil.ToFloat64(h.metrics.requests.WithLabelValues("/", "404")); got != 3 {
		t.Errorf("requests_total{path=\"/\",code=\"404\"} = %v, want 3", got)
	}
}
