package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trifork/loki-label-proxy/internal/enforce"
)

const (
	label      = "tenant_namespace"
	headerName = "X-Tenant-Namespace"
	value      = "team-a-prod"
)

// upstream records what the proxy actually forwarded.
type upstream struct {
	gotQuery url.Values
	gotPath  string
	gotHdr   http.Header
}

func newTestHandler(t *testing.T) (*Handler, *upstream) {
	t.Helper()

	up := &upstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("upstream could not parse form: %v", err)
		}
		up.gotQuery = r.Form
		up.gotPath = r.URL.Path
		up.gotHdr = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	enforcer, err := enforce.New(label, true)
	if err != nil {
		t.Fatalf("enforce.New: %v", err)
	}
	h, err := New(Config{Upstream: target, Enforcer: enforcer, HeaderName: headerName})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, up
}

func TestGetQueryIsEnforced(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, `/loki/api/v1/query_range?query={app%3D"x"}&limit=100`, "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := up.gotQuery.Get("query"), `{app="x", tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("forwarded query = %s, want %s", got, want)
	}
	// Unrelated parameters must survive the rewrite.
	if got := up.gotQuery.Get("limit"); got != "100" {
		t.Errorf("forwarded limit = %q, want 100", got)
	}
}

// Grafana switches to POST for long queries, so the form body path has to be
// enforced just as the query string is.
func TestPostFormQueryIsEnforced(t *testing.T) {
	h, up := newTestHandler(t)

	body := url.Values{"query": {`{app="x"}`}, "limit": {"100"}}.Encode()
	rec := do(t, h, http.MethodPost, "/loki/api/v1/query_range", body, value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := up.gotQuery.Get("query"), `{app="x", tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("forwarded query = %s, want %s", got, want)
	}
}

// A query smuggled into the URL of a POST must not survive un-enforced.
func TestPostDoesNotLeaveQueryStringUnenforced(t *testing.T) {
	h, up := newTestHandler(t)

	body := url.Values{"query": {`{app="x"}`}}.Encode()
	rec := do(t, h, http.MethodPost, `/loki/api/v1/query_range?query={app%3D"sneaky"}`, body, value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	for _, q := range up.gotQuery["query"] {
		if !strings.Contains(q, `tenant_namespace="`+value+`"`) {
			t.Errorf("forwarded an un-enforced query: %s", q)
		}
	}
}

func TestMissingHeaderIsRejected(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, `/loki/api/v1/query_range?query={app%3D"x"}`, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if up.gotPath != "" {
		t.Errorf("request reached upstream at %s, want no forwarding", up.gotPath)
	}
}

func TestMalformedHeaderIsRejected(t *testing.T) {
	for _, v := range []string{`ns", foo="bar`, "UPPER", "has space", "-leading"} {
		t.Run(v, func(t *testing.T) {
			h, up := newTestHandler(t)
			rec := do(t, h, http.MethodGet, `/loki/api/v1/query_range?query={app%3D"x"}`, "", v)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if up.gotPath != "" {
				t.Errorf("request reached upstream at %s, want no forwarding", up.gotPath)
			}
		})
	}
}

func TestCallerSuppliedMatcherIsRejected(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, `/loki/api/v1/query_range?query={tenant_namespace%3D"other-ns"}`, "", value)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if up.gotPath != "" {
		t.Errorf("request reached upstream at %s, want no forwarding", up.gotPath)
	}
}

func TestUnparseableQueryDoesNotReachUpstream(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, `/loki/api/v1/query_range?query={app%3D"x"`, "", value)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if up.gotPath != "" {
		t.Errorf("request reached upstream at %s, want no forwarding", up.gotPath)
	}
}

func TestUnknownPathIsNotProxied(t *testing.T) {
	h, up := newTestHandler(t)

	for _, path := range []string{"/loki/api/v1/delete", "/ready", "/config", "/"} {
		rec := do(t, h, http.MethodGet, path, "", value)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if got, want := rec.Body.String(), `{"status":"error","error":"not found"}`; got != want {
			t.Errorf("%s: body = %s, want %s", path, got, want)
		}
	}
	if up.gotPath != "" {
		t.Errorf("request reached upstream at %s, want no forwarding", up.gotPath)
	}
}

func TestSeriesEnforcesEveryMatcher(t *testing.T) {
	h, up := newTestHandler(t)

	q := url.Values{"match[]": {`{app="x"}`, `{app="y"}`}}.Encode()
	rec := do(t, h, http.MethodGet, "/loki/api/v1/series?"+q, "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := up.gotQuery["match[]"]
	want := []string{
		`{app="x", tenant_namespace="team-a-prod"}`,
		`{app="y", tenant_namespace="team-a-prod"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %d matchers, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("match[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// With no match[] of its own, the series endpoint would otherwise enumerate
// every stream in the tenant.
func TestSeriesWithoutMatcherGetsOneInjected(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/loki/api/v1/series", "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := up.gotQuery.Get("match[]"), `{tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("injected match[] = %s, want %s", got, want)
	}
}

// Left unenforced, this endpoint lists every other tenant's namespace.
func TestLabelsWithoutQueryGetsSelectorInjected(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/loki/api/v1/labels", "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := up.gotQuery.Get("query"), `{tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("injected query = %s, want %s", got, want)
	}
}

func TestLabelValuesForEnforcedLabelIsAnsweredDirectly(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/loki/api/v1/label/"+label+"/values", "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if want := `{"status":"success","data":["team-a-prod"]}`; rec.Body.String() != want {
		t.Errorf("body = %s, want %s", rec.Body.String(), want)
	}
	if up.gotPath != "" {
		t.Errorf("request reached upstream at %s, want answered locally", up.gotPath)
	}
}

func TestLabelValuesForOtherLabelIsEnforced(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/loki/api/v1/label/app/values", "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := up.gotQuery.Get("query"), `{tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("injected query = %s, want %s", got, want)
	}
}

// The datasource sets the header; a second copy from the client must not reach
// Loki, where it could be read as something else.
func TestHeaderIsStrippedBeforeForwarding(t *testing.T) {
	h, up := newTestHandler(t)

	do(t, h, http.MethodGet, `/loki/api/v1/query_range?query={app%3D"x"}`, "", value)
	if got := up.gotHdr.Get(headerName); got != "" {
		t.Errorf("upstream saw %s = %q, want it stripped", headerName, got)
	}
}

func do(t *testing.T, h *Handler, method, target, body, headerValue string) *httptest.ResponseRecorder {
	t.Helper()

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if headerValue != "" {
		r.Header.Set(headerName, headerValue)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// Grafana sends a bare "query=" on its metadata calls. Treating present-but-empty
// as a query to parse yields "unexpected $end", which surfaces in Grafana as an
// error popup even though the data queries themselves work.
func TestEmptyQueryParamIsTreatedAsAbsent(t *testing.T) {
	for _, target := range []string{
		"/loki/api/v1/labels?query=",
		"/loki/api/v1/labels?query=&start=1&end=2",
		"/loki/api/v1/labels?query=%20",
		"/loki/api/v1/label/app/values?query=",
	} {
		t.Run(target, func(t *testing.T) {
			h, up := newTestHandler(t)

			rec := do(t, h, http.MethodGet, target, "", value)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			if got, want := up.gotQuery.Get("query"), `{tenant_namespace="team-a-prod"}`; got != want {
				t.Errorf("forwarded query = %q, want the injected selector %s", got, want)
			}
		})
	}
}

func TestEmptyMatchIsTreatedAsAbsent(t *testing.T) {
	h, up := newTestHandler(t)

	rec := do(t, h, http.MethodGet, "/loki/api/v1/series?match[]=", "", value)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := up.gotQuery.Get("match[]"), `{tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("forwarded match[] = %q, want %s", got, want)
	}
}

// Loki requires a query on these endpoints, but enforcing that here would make
// the proxy stricter than the thing it fronts, and Grafana issues speculative
// calls with no query yet. Inject the selector and let Loki answer for its own
// contract -- the request is still scoped, which is all this proxy owes.
func TestEmptyQueryOnQueryEndpointsGetsSelectorInjected(t *testing.T) {
	for _, path := range []string{
		"/loki/api/v1/query_range",
		"/loki/api/v1/query",
		"/loki/api/v1/index/stats",
		"/loki/api/v1/index/volume",
		"/loki/api/v1/index/volume_range",
		"/loki/api/v1/patterns",
		"/loki/api/v1/detected_labels",
		"/loki/api/v1/detected_fields",
	} {
		for _, target := range []string{path, path + "?query="} {
			t.Run(target, func(t *testing.T) {
				h, up := newTestHandler(t)

				rec := do(t, h, http.MethodGet, target, "", value)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
				}
				if got, want := up.gotQuery.Get("query"), `{tenant_namespace="team-a-prod"}`; got != want {
					t.Errorf("forwarded query = %q, want %s", got, want)
				}
			})
		}
	}
}
