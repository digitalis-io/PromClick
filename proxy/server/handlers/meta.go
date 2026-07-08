package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
)

// MetaQuerier performs metadata queries against ClickHouse HTTP interface.
type MetaQuerier struct {
	Addr       string
	Database   string
	User       string
	Password   string
	HTTPClient *http.Client

	// Mode "otel" makes the metadata endpoints read the OpenTelemetry
	// ClickHouse-exporter metric tables (Tables) directly instead of the
	// Prometheus time_series table, mirroring the translator's renderOTel:
	// label names/values come from the sanitised merge of ResourceAttributes +
	// Attributes, metric names from a sanitised MetricName.
	Mode   string
	Tables []string
}

// otelMetaLookbackHours bounds metadata scans to recently-active series so
// full-table DISTINCT scans over the _dist tables stay cheap.
const otelMetaLookbackHours = 6

// otelSanitize sanitises an identifier expression to a valid Prometheus name.
func otelSanitize(expr string) string {
	return fmt.Sprintf("replaceRegexpAll(%s, '[^a-zA-Z0-9_]', '_')", expr)
}

// otelLabelsExpr is the sanitised merge of ResourceAttributes + Attributes as a
// Map(String,String), matching the label set renderOTel emits at query time.
const otelLabelsExpr = "mapFromArrays(" +
	"arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(mapConcat(ResourceAttributes, Attributes))), " +
	"mapValues(mapConcat(ResourceAttributes, Attributes)))"

// otelUnion builds a UNION ALL subquery over the configured OTel tables,
// selecting selectCols and applying the lookback window plus any extra
// predicate (already escaped).
func (m *MetaQuerier) otelUnion(selectCols, whereExtra string) string {
	arms := make([]string, 0, len(m.Tables))
	for _, t := range m.Tables {
		w := fmt.Sprintf("TimeUnix >= now() - toIntervalHour(%d)", otelMetaLookbackHours)
		if whereExtra != "" {
			w += " AND " + whereExtra
		}
		arms = append(arms, fmt.Sprintf("SELECT %s FROM %s WHERE %s", selectCols, t, w))
	}
	return "(" + strings.Join(arms, " UNION ALL ") + ")"
}

// chEscape escapes a literal for inline ClickHouse SQL. Backslash is the
// ClickHouse string-literal escape character, so it must be doubled *before*
// escaping the single quote — otherwise a value ending in a backslash would
// escape the closing quote and allow SQL injection.
func chEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "'", "\\'")
}

func (m *MetaQuerier) query(ctx context.Context, sql string) ([][]byte, error) {
	u := strings.TrimRight(m.Addr, "/") + "/?database=" + m.Database + "&default_format=JSONEachRow"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(sql))
	if err != nil {
		return nil, err
	}
	if m.User != "" {
		req.SetBasicAuth(m.User, m.Password)
	}
	req.Header.Set("Content-Type", "text/plain")

	client := m.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("clickhouse HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rows [][]byte
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		rows = append(rows, raw)
	}
	return rows, nil
}

// Labels returns all distinct label names from the tags table.
func (m *MetaQuerier) Labels(ctx context.Context, tagsTable, labelsCol string) ([]string, error) {
	var sql string
	if m.Mode == "otel" {
		// Distinct sanitised keys of the merged ResourceAttributes+Attributes map.
		sql = "SELECT DISTINCT arrayJoin(mapKeys(" + otelLabelsExpr + ")) AS name FROM " +
			m.otelUnion("ResourceAttributes, Attributes", "") + " ORDER BY name"
	} else {
		sql = fmt.Sprintf("SELECT DISTINCT arrayJoin(JSONExtractKeys(%s)) AS name FROM %s ORDER BY name",
			labelsCol, tagsTable)
	}
	rows, err := m.query(ctx, sql)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, row := range rows {
		var v struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(row, &v); err == nil && v.Name != "" {
			result = append(result, v.Name)
		}
	}
	// Always include __name__
	found := false
	for _, l := range result {
		if l == "__name__" {
			found = true
			break
		}
	}
	if !found {
		result = append([]string{"__name__"}, result...)
	}
	return result, nil
}

// LabelValues returns distinct values for a given label.
func (m *MetaQuerier) LabelValues(ctx context.Context, labelName, tagsTable, metricNameCol, labelsCol string) ([]string, error) {
	var sql string
	switch {
	case m.Mode == "otel" && labelName == "__name__":
		sql = "SELECT DISTINCT " + otelSanitize("MetricName") + " AS value FROM " +
			m.otelUnion("MetricName", "") + " ORDER BY value"
	case m.Mode == "otel":
		// Sanitised value of the requested key from the merged attribute map.
		key := chEscape(labelName)
		sql = "SELECT DISTINCT value FROM (SELECT (" + otelLabelsExpr + ")['" + key + "'] AS value FROM " +
			m.otelUnion("ResourceAttributes, Attributes", "") + ") WHERE value != '' ORDER BY value"
	case labelName == "__name__":
		sql = fmt.Sprintf("SELECT DISTINCT %s AS value FROM %s ORDER BY value",
			metricNameCol, tagsTable)
	default:
		sql = fmt.Sprintf("SELECT DISTINCT JSONExtractString(%s, '%s') AS value FROM %s WHERE value != '' ORDER BY value",
			labelsCol, labelName, tagsTable)
	}
	rows, err := m.query(ctx, sql)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, row := range rows {
		var v struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(row, &v); err == nil && v.Value != "" {
			result = append(result, v.Value)
		}
	}
	return result, nil
}

// colMatchCond renders a single matcher as a ClickHouse boolean condition on
// the given column expression. Regex matchers are anchored to match Prometheus
// semantics. Values are escaped for inline SQL.
func colMatchCond(col string, mt labels.MatchType, value string) string {
	v := chEscape(value)
	switch mt {
	case labels.MatchNotEqual:
		return fmt.Sprintf("%s != '%s'", col, v)
	case labels.MatchRegexp:
		return fmt.Sprintf("match(%s, '^(?:%s)$')", col, v)
	case labels.MatchNotRegexp:
		return fmt.Sprintf("NOT match(%s, '^(?:%s)$')", col, v)
	default: // MatchEqual
		return fmt.Sprintf("%s = '%s'", col, v)
	}
}

// chMatcherWhere builds a WHERE clause (without the leading WHERE) applying
// every matcher against the Prometheus-mode time_series schema: __name__ maps
// to the metric-name column, other labels to JSONExtractString(labels, name).
func chMatcherWhere(all []*labels.Matcher, metricNameCol, labelsCol string) string {
	if len(all) == 0 {
		return "1"
	}
	conds := make([]string, 0, len(all))
	for _, mt := range all {
		col := fmt.Sprintf("JSONExtractString(%s, '%s')", labelsCol, chEscape(mt.Name))
		if mt.Name == labels.MetricName {
			col = metricNameCol
		}
		conds = append(conds, colMatchCond(col, mt.Type, mt.Value))
	}
	return strings.Join(conds, " AND ")
}

// otelMatcherWhere builds a WHERE clause for OTel mode: __name__ maps to the
// sanitised MetricName, other labels to the merged attribute map.
func otelMatcherWhere(all []*labels.Matcher) string {
	if len(all) == 0 {
		return "1"
	}
	conds := make([]string, 0, len(all))
	for _, mt := range all {
		col := "(" + otelLabelsExpr + ")['" + chEscape(mt.Name) + "']"
		if mt.Name == labels.MetricName {
			col = otelSanitize("MetricName")
		}
		conds = append(conds, colMatchCond(col, mt.Type, mt.Value))
	}
	return strings.Join(conds, " AND ")
}

// SeriesMatch queries ClickHouse for the label sets matching a single selector,
// applying every matcher. Used as the fallback when the label cache cannot
// answer (cache disabled/unloaded, or a regex/negated metric-name matcher).
func (m *MetaQuerier) SeriesMatch(ctx context.Context, sel parsedSelector, tagsTable, metricNameCol, labelsCol string, limit int) ([]map[string]string, error) {
	var sql string
	if m.Mode == "otel" {
		where := otelMatcherWhere(sel.All)
		sql = "SELECT DISTINCT " + otelSanitize("MetricName") + " AS metric_name, " +
			otelLabelsExpr + " AS labels FROM " +
			m.otelUnion("MetricName, ResourceAttributes, Attributes", where) +
			fmt.Sprintf(" LIMIT %d", limit)
	} else {
		where := chMatcherWhere(sel.All, metricNameCol, labelsCol)
		sql = fmt.Sprintf(
			"SELECT %s AS metric_name, %s AS labels FROM %s WHERE %s ORDER BY unix_milli DESC LIMIT 1 BY fingerprint LIMIT %d",
			metricNameCol, labelsCol, tagsTable, where, limit,
		)
	}

	rows, err := m.query(ctx, sql)
	if err != nil {
		return nil, err
	}

	results := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		var v struct {
			MetricName string          `json:"metric_name"`
			Labels     json.RawMessage `json:"labels"`
		}
		if err := json.Unmarshal(row, &v); err != nil {
			continue
		}
		lbls := decodeLabels(v.Labels)
		labelSet := make(map[string]string, len(lbls)+1)
		labelSet["__name__"] = v.MetricName
		for k, val := range lbls {
			labelSet[k] = val
		}
		results = append(results, labelSet)
	}
	return results, nil
}

// decodeLabels decodes the JSONEachRow "labels" column, which is either a JSON
// object (Map columns, OTel mode) or a JSON-encoded string containing an object
// (the default String labels column). Returns an empty map on anything else.
func decodeLabels(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			_ = json.Unmarshal([]byte(s), &out)
		}
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// parseCount extracts a count value from CH JSON row (count() returns string in JSONEachRow).
func parseCount(row []byte) float64 {
	var v struct {
		C json.Number `json:"c"`
	}
	json.Unmarshal(row, &v)
	f, _ := v.C.Float64()
	return f
}

// TSDBStatus returns cardinality stats from ClickHouse.
func (m *MetaQuerier) TSDBStatus(ctx context.Context, tagsTable, metricNameCol, labelsCol, samplesTable, tsCol string) (map[string]interface{}, error) {
	if m.Mode == "otel" {
		return m.tsdbStatusOTel(ctx)
	}
	// Total series (count distinct fingerprints)
	rows, _ := m.query(ctx, fmt.Sprintf("SELECT count(DISTINCT fingerprint) AS c FROM %s", tagsTable))
	var numSeries float64
	if len(rows) > 0 {
		numSeries = parseCount(rows[0])
	}

	// Total samples
	rows, _ = m.query(ctx, fmt.Sprintf("SELECT count() AS c FROM %s", samplesTable))
	var numSamples float64
	if len(rows) > 0 {
		numSamples = parseCount(rows[0])
	}

	// Top 10 metrics by series count
	rows, _ = m.query(ctx, fmt.Sprintf(
		"SELECT %s AS name, count(DISTINCT fingerprint) AS c FROM %s GROUP BY name ORDER BY c DESC LIMIT 10",
		metricNameCol, tagsTable))
	var topMetrics []map[string]interface{}
	for _, row := range rows {
		var v struct {
			Name string      `json:"name"`
			C    json.Number `json:"c"`
		}
		json.Unmarshal(row, &v)
		cnt, _ := v.C.Float64()
		topMetrics = append(topMetrics, map[string]interface{}{"name": v.Name, "seriesCount": cnt})
	}

	// Top 10 label names by value count
	rows, _ = m.query(ctx, fmt.Sprintf(
		"SELECT name, count() AS c FROM (SELECT DISTINCT arrayJoin(JSONExtractKeys(%s)) AS name FROM %s) GROUP BY name ORDER BY c DESC LIMIT 10",
		labelsCol, tagsTable))
	var topLabels []map[string]interface{}
	for _, row := range rows {
		var v struct {
			Name string      `json:"name"`
			C    json.Number `json:"c"`
		}
		json.Unmarshal(row, &v)
		cnt, _ := v.C.Float64()
		topLabels = append(topLabels, map[string]interface{}{"name": v.Name, "seriesCount": cnt})
	}

	return map[string]interface{}{
		"numSeries":     numSeries,
		"numSamples":    numSamples,
		"topMetrics":    topMetrics,
		"topLabelNames": topLabels,
	}, nil
}

// tsdbStatusOTel returns best-effort cardinality stats for OTel mode. There is
// no fingerprint/series table, so numSeries approximates distinct metric names
// and numSamples counts datapoints in the lookback window.
func (m *MetaQuerier) tsdbStatusOTel(ctx context.Context) (map[string]interface{}, error) {
	countRows, _ := m.query(ctx, "SELECT count() AS c FROM "+m.otelUnion("1", ""))
	var numSamples float64
	if len(countRows) > 0 {
		numSamples = parseCount(countRows[0])
	}

	sanMetric := otelSanitize("MetricName")
	metricRows, _ := m.query(ctx, "SELECT name, count() AS c FROM (SELECT "+sanMetric+
		" AS name FROM "+m.otelUnion("MetricName", "")+") GROUP BY name ORDER BY c DESC LIMIT 10")
	var topMetrics []map[string]interface{}
	for _, row := range metricRows {
		var v struct {
			Name string      `json:"name"`
			C    json.Number `json:"c"`
		}
		json.Unmarshal(row, &v)
		cnt, _ := v.C.Float64()
		topMetrics = append(topMetrics, map[string]interface{}{"name": v.Name, "seriesCount": cnt})
	}

	distinctRows, _ := m.query(ctx, "SELECT count(DISTINCT "+sanMetric+") AS c FROM "+m.otelUnion("MetricName", ""))
	var numSeries float64
	if len(distinctRows) > 0 {
		numSeries = parseCount(distinctRows[0])
	}

	return map[string]interface{}{
		"numSeries":     numSeries,
		"numSamples":    numSamples,
		"topMetrics":    topMetrics,
		"topLabelNames": []map[string]interface{}{},
	}, nil
}
