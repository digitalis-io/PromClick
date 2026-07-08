package handlers

import (
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	nativech "github.com/PromClick/PromClick/proxy/clickhouse"
)

// parsedSelector is one match[] selector split into its metric name and the
// remaining label matchers.
type parsedSelector struct {
	// MetricName is the value of the __name__ matcher when it is an equality
	// match, else "" (e.g. a regex name matcher or a bare label selector).
	MetricName string
	// Matchers holds every matcher except a leading __name__= equality, mapped
	// to the native label-cache matcher form.
	Matchers []nativech.LabelMatcher
	// All is the raw matcher set (including __name__), for the ClickHouse
	// fallback path.
	All []*labels.Matcher
}

// matchTypeOp maps a Prometheus match type to the label-cache operator string.
func matchTypeOp(t labels.MatchType) string {
	switch t {
	case labels.MatchNotEqual:
		return "!="
	case labels.MatchRegexp:
		return "=~"
	case labels.MatchNotRegexp:
		return "!~"
	default:
		return "="
	}
}

// parseSelector parses a single match[] expression such as `up{job="api"}` or
// `{__name__=~"node_.*"}` into a metric name and label matchers, using the
// Prometheus parser (the same one the translator uses).
func parseSelector(expr string) (parsedSelector, error) {
	ms, err := parser.ParseMetricSelector(expr)
	if err != nil {
		return parsedSelector{}, err
	}
	ps := parsedSelector{All: ms}
	for _, m := range ms {
		if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
			ps.MetricName = m.Value
			continue
		}
		ps.Matchers = append(ps.Matchers, nativech.LabelMatcher{
			Name:  m.Name,
			Op:    matchTypeOp(m.Type),
			Value: m.Value,
		})
	}
	return ps, nil
}
