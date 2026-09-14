package proxy

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Reasons a request was refused, used as the `reason` label on rejectedTotal.
const (
	reasonMissingHeader   = "missing_header"
	reasonMalformedHeader = "malformed_header"
	reasonLabelPresent    = "label_present"
	reasonUnparseable     = "unparseable_query"
)

type metrics struct {
	requests *prometheus.CounterVec
	rejected *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loki_label_proxy_requests_total",
			Help: "Requests handled, by request path and response status code.",
		}, []string{"path", "code"}),
		// Split out from requests_total because this is the series worth
		// alerting on: a rising rejection rate is either a tenant probing the
		// boundary or a datasource that has lost its header.
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loki_label_proxy_rejected_total",
			Help: "Requests refused before reaching Loki, by reason.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.requests, m.rejected)
	return m
}

func (m *metrics) observe(path string, code int) {
	m.requests.WithLabelValues(path, strconv.Itoa(code)).Inc()
}

func (m *metrics) reject(reason string) {
	m.rejected.WithLabelValues(reason).Inc()
}

// statusRecorder captures the status code for observe, since ResponseWriter
// does not expose it.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}
