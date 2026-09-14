package enforce

import (
	"errors"
	"testing"
)

const (
	label = "tenant_namespace"
	value = "team-a-prod"
)

func TestQuery(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare selector",
			in:   `{app="x"}`,
			want: `{app="x", tenant_namespace="team-a-prod"}`,
		},
		{
			name: "log pipeline is preserved",
			in:   `{app="x"} |= "boom" | json`,
			want: `{app="x", tenant_namespace="team-a-prod"} |= "boom" | json`,
		},
		{
			// The case that defeats regex rewriting: the "{{ }}" of a
			// line_format template is not a stream selector.
			name: "line_format template braces are not a selector",
			in:   `{app="x"} |= "e" | json | line_format "{{.a}}"`,
			want: `{app="x", tenant_namespace="team-a-prod"} |= "e" | json | line_format "{{.a}}"`,
		},
		{
			name: "label_format template braces are not a selector",
			in:   `{app="x"} | label_format dst="{{.src}}"`,
			want: `{app="x", tenant_namespace="team-a-prod"} | label_format dst="{{.src}}"`,
		},
		{
			// Metric queries hide the selector inside a range aggregation.
			name: "metric query",
			in:   `sum(rate({app="x"}[5m])) by (pod)`,
			want: `sum by (pod)(rate({app="x", tenant_namespace="team-a-prod"}[5m]))`,
		},
		{
			name: "nested aggregation",
			in:   `topk(5, sum by (pod) (count_over_time({app="x"}[1h])))`,
			want: `topk(5,sum by (pod)(count_over_time({app="x", tenant_namespace="team-a-prod"}[1h])))`,
		},
		{
			// Both sides must be constrained, or the unconstrained one leaks.
			name: "binary operation constrains both sides",
			in:   `sum(rate({app="x"}[5m])) / sum(rate({app="y"}[5m]))`,
			want: `(sum(rate({app="x", tenant_namespace="team-a-prod"}[5m])) / sum(rate({app="y", tenant_namespace="team-a-prod"}[5m])))`,
		},
		{
			name: "selector with no matchers of its own",
			in:   `{app=~".+"}`,
			want: `{app=~".+", tenant_namespace="team-a-prod"}`,
		},
	}

	enforcer := mustNew(t, true)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := enforcer.Query(tt.in, value)
			if err != nil {
				t.Fatalf("Query(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Query(%q)\n got: %s\nwant: %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestQueryRejectsCallerSuppliedMatcher(t *testing.T) {
	// Every one of these is an attempt to widen or redirect the enforced scope.
	for _, in := range []string{
		`{tenant_namespace="other-ns"}`,
		`{app="x", tenant_namespace="other-ns"}`,
		`{tenant_namespace=~".+"}`,
		`sum(rate({tenant_namespace="other-ns"}[5m]))`,
		// Only the second selector cheats; the query must still be rejected.
		`sum(rate({app="x"}[5m])) / sum(rate({tenant_namespace="other-ns"}[5m]))`,
	} {
		t.Run(in, func(t *testing.T) {
			enforcer := mustNew(t, true)
			if _, err := enforcer.Query(in, value); !errors.Is(err, ErrLabelPresent) {
				t.Errorf("Query(%q) error = %v, want ErrLabelPresent", in, err)
			}
		})
	}
}

// A negated-only selector is refused by Loki's own grammar, before we ever get
// to look for the enforced label. It is still refused, which is what matters.
func TestQueryRejectsNegatedOnlySelector(t *testing.T) {
	enforcer := mustNew(t, true)
	if _, err := enforcer.Query(`{tenant_namespace!="team-a-prod"}`, value); err == nil {
		t.Error("Query with negated-only selector succeeded, want error")
	}
}

func TestQueryReplacesCallerSuppliedMatcherWhenPermitted(t *testing.T) {
	enforcer := mustNew(t, false)
	got, err := enforcer.Query(`{app="x", tenant_namespace="other-ns"}`, value)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	want := `{app="x", tenant_namespace="team-a-prod"}`
	if got != want {
		t.Errorf("Query\n got: %s\nwant: %s", got, want)
	}
}

func TestQueryRejectsUnparseable(t *testing.T) {
	// A query we cannot parse is a query we cannot constrain, so it must never
	// reach Loki.
	for _, in := range []string{
		`{app="x"`,
		`not a query at all`,
		``,
		`{}`,
	} {
		t.Run(in, func(t *testing.T) {
			enforcer := mustNew(t, true)
			if _, err := enforcer.Query(in, value); err == nil {
				t.Errorf("Query(%q) succeeded, want error", in)
			}
		})
	}
}

func TestMatchers(t *testing.T) {
	enforcer := mustNew(t, true)

	got, err := enforcer.Matchers(`{app="x"}`, value)
	if err != nil {
		t.Fatalf("Matchers returned error: %v", err)
	}
	if want := `{app="x", tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("Matchers\n got: %s\nwant: %s", got, want)
	}

	if _, err := enforcer.Matchers(`{tenant_namespace="other-ns"}`, value); !errors.Is(err, ErrLabelPresent) {
		t.Errorf("Matchers with caller-supplied matcher: error = %v, want ErrLabelPresent", err)
	}
}

func TestSelector(t *testing.T) {
	enforcer := mustNew(t, true)
	got, err := enforcer.Selector(value)
	if err != nil {
		t.Fatalf("Selector returned error: %v", err)
	}
	if want := `{tenant_namespace="team-a-prod"}`; got != want {
		t.Errorf("Selector = %s, want %s", got, want)
	}
}

func TestRejectsEmptyValue(t *testing.T) {
	enforcer := mustNew(t, true)
	if _, err := enforcer.Query(`{app="x"}`, ""); err == nil {
		t.Error("Query with empty value succeeded, want error")
	}
	if _, err := enforcer.Selector(""); err == nil {
		t.Error("Selector with empty value succeeded, want error")
	}
}

func TestNewRejectsInvalidLabel(t *testing.T) {
	for _, in := range []string{"", "not a label", "1abc", "a-b"} {
		if _, err := New(in, true); err == nil {
			t.Errorf("New(%q) succeeded, want error", in)
		}
	}
}

func mustNew(t *testing.T, errorOnReplace bool) *Enforcer {
	t.Helper()
	e, err := New(label, errorOnReplace)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}
