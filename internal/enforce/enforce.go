// Package enforce rewrites LogQL queries so that every stream selector they
// contain carries a fixed label matcher.
//
// Rewriting is done against Loki's own LogQL grammar rather than by pattern
// matching on the query text. That matters: a selector can appear inside an
// aggregation, on either side of a binary operation, or alongside a
// line_format template whose "{{ }}" braces look a lot like a stream selector
// to a regular expression. Parsing sidesteps all of it.
package enforce

import (
	"errors"
	"fmt"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
)

// ErrLabelPresent is returned when the caller's query already constrains the
// enforced label and the Enforcer was built with errorOnReplace set.
var ErrLabelPresent = errors.New("query already contains the enforced label")

// Enforcer injects a single label matcher into LogQL queries and selectors.
type Enforcer struct {
	label          string
	errorOnReplace bool
}

// New returns an Enforcer for the named label. errorOnReplace decides what
// happens when the caller supplies their own matcher for that label: reject the
// query, or silently replace it.
//
// Rejecting is the safer default. Silently replacing turns a deliberate
// cross-tenant query into an innocuous one, which hides the attempt; failing
// loudly surfaces it.
func New(label string, errorOnReplace bool) (*Enforcer, error) {
	if label == "" {
		return nil, errors.New("enforce: label must not be empty")
	}
	// IsValidLegacy, not IsValid: the latter consults a package-level
	// validation scheme that now defaults to UTF-8 and accepts almost anything,
	// and which a transitive dependency could change underneath us.
	if !model.LabelName(label).IsValidLegacy() {
		return nil, fmt.Errorf("enforce: %q is not a valid label name", label)
	}
	return &Enforcer{label: label, errorOnReplace: errorOnReplace}, nil
}

// Label returns the label name this Enforcer constrains.
func (e *Enforcer) Label() string { return e.label }

// Query rewrites a complete LogQL expression, log or metric, so that every
// stream selector in it matches label=value.
func (e *Enforcer) Query(query, value string) (string, error) {
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		return "", fmt.Errorf("enforce: %w", err)
	}
	matcher, err := e.matcher(value)
	if err != nil {
		return "", err
	}

	var present bool
	// Walk visits every node; MatchersExpr is the only one holding a stream
	// selector, and AppendMatchers mutates it in place. The bool asks whether
	// to descend into the node's children, and every selector in the tree has
	// to be rewritten, so it is always true.
	expr.Walk(func(node syntax.Expr) bool {
		selector, ok := node.(*syntax.MatchersExpr)
		if !ok {
			return true
		}
		kept, found := strip(selector.Mts, e.label)
		present = present || found
		selector.Mts = kept
		selector.AppendMatchers([]*labels.Matcher{matcher})
		return true
	})

	if present && e.errorOnReplace {
		return "", ErrLabelPresent
	}
	return expr.String(), nil
}

// Matchers rewrites a bare stream selector, as passed to the series endpoint's
// match[] parameter. Unlike Query it rejects anything carrying a log pipeline,
// because the series endpoint does not accept one.
func (e *Enforcer) Matchers(selector, value string) (string, error) {
	parsed, err := syntax.ParseMatchers(selector, true)
	if err != nil {
		return "", fmt.Errorf("enforce: %w", err)
	}
	matcher, err := e.matcher(value)
	if err != nil {
		return "", err
	}

	kept, present := strip(parsed, e.label)
	if present && e.errorOnReplace {
		return "", ErrLabelPresent
	}
	return (&syntax.MatchersExpr{Mts: append(kept, matcher)}).String(), nil
}

// Selector returns the bare selector {label="value"}, for endpoints called
// without any query of their own.
func (e *Enforcer) Selector(value string) (string, error) {
	matcher, err := e.matcher(value)
	if err != nil {
		return "", err
	}
	return (&syntax.MatchersExpr{Mts: []*labels.Matcher{matcher}}).String(), nil
}

func (e *Enforcer) matcher(value string) (*labels.Matcher, error) {
	if value == "" {
		return nil, errors.New("enforce: value must not be empty")
	}
	m, err := labels.NewMatcher(labels.MatchEqual, e.label, value)
	if err != nil {
		return nil, fmt.Errorf("enforce: %w", err)
	}
	return m, nil
}

// strip removes every matcher for label, reporting whether any was found.
func strip(matchers []*labels.Matcher, label string) ([]*labels.Matcher, bool) {
	kept := make([]*labels.Matcher, 0, len(matchers)+1)
	var found bool
	for _, m := range matchers {
		if m.Name == label {
			found = true
			continue
		}
		kept = append(kept, m)
	}
	return kept, found
}
