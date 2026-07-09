package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	promcfg "github.com/PromClick/PromClick/config"
	proxycfg "github.com/PromClick/PromClick/proxy/config"
)

// fakeCH returns an httptest server that mimics the ClickHouse HTTP interface
// for the metadata/series query paths. It routes on SQL content and emits
// JSONEachRow rows. The `labels` column is emitted as a JSON-encoded string,
// exactly as a ClickHouse String column is serialised.
func fakeCH(t *testing.T) *httptest.Server {
	t.Helper()
	series := []struct {
		name   string
		labels map[string]string
	}{
		{"up", map[string]string{"job": "api", "instance": "h1"}},
		{"up", map[string]string{"job": "api", "instance": "h2"}},
		{"up", map[string]string{"job": "db", "instance": "h3"}},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sql := string(body)
		enc := json.NewEncoder(w)

		switch {
		// SeriesMatch: SELECT metric_name AS metric_name, labels AS labels ...
		case strings.Contains(sql, "AS labels"):
			for _, s := range series {
				if !sqlMatches(sql, s.name, s.labels) {
					continue
				}
				// Emit labels as a JSON string, like a CH String column.
				lb, _ := json.Marshal(s.labels)
				_ = enc.Encode(map[string]string{
					"metric_name": s.name,
					"labels":      string(lb),
				})
			}
		// LabelValues(__name__): SELECT DISTINCT metric_name AS value ...
		case strings.Contains(sql, "AS value"):
			for _, v := range []string{"up", "process_cpu"} {
				_ = enc.Encode(map[string]string{"value": v})
			}
		default:
			// empty result
		}
	}))
}

var (
	reNameEq  = regexp.MustCompile(`metric_name = '([^']*)'`)
	reLabelEq = regexp.MustCompile(`JSONExtractString\(labels, '(\w+)'\) = '([^']*)'`)
)

// sqlMatches applies the equality conditions found in the SQL (the WHERE that
// chMatcherWhere built) to one series, so the fake CH filters like the real one.
func sqlMatches(sql, name string, lbls map[string]string) bool {
	if m := reNameEq.FindStringSubmatch(sql); m != nil && m[1] != name {
		return false
	}
	for _, m := range reLabelEq.FindAllStringSubmatch(sql, -1) {
		if lbls[m[1]] != m[2] {
			return false
		}
	}
	return true
}

func newTestHandler(chURL string) *Handler {
	return &Handler{
		Cfg: &proxycfg.Config{},
		PromCfg: &promcfg.Config{
			Schema: promcfg.SchemaConfig{
				SamplesTable:    "samples",
				TimeSeriesTable: "time_series",
				Columns: promcfg.ColumnConfig{
					MetricName: "metric_name",
					Timestamp:  "unix_milli",
					Value:      "value",
					Labels:     "labels",
				},
			},
		},
		Meta: &MetaQuerier{
			Addr:       chURL,
			Database:   "metrics",
			HTTPClient: http.DefaultClient,
		},
		// Pool nil → matchedSeries takes the ClickHouse fallback path.
	}
}

func decodeAPI(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var resp struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("bad JSON envelope: %v (%s)", err, body)
	}
	if resp.Status != "success" {
		t.Fatalf("status = %q, want success (%s)", resp.Status, body)
	}
	return map[string]json.RawMessage{"data": resp.Data}
}

func TestSeriesAppliesLabelMatchers(t *testing.T) {
	srv := fakeCH(t)
	defer srv.Close()
	h := newTestHandler(srv.URL)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/series?match[]="+url.QueryEscape(`up{job="api"}`), nil)
	w := httptest.NewRecorder()
	h.Series(w, req)

	data := decodeAPI(t, w.Body.Bytes())
	var sets []map[string]string
	if err := json.Unmarshal(data["data"], &sets); err != nil {
		t.Fatalf("decode series: %v", err)
	}
	// Fake CH returns 3 series; the job="api" matcher must drop the db one.
	if len(sets) != 2 {
		t.Fatalf("got %d series, want 2 (job=api only): %+v", len(sets), sets)
	}
	for _, s := range sets {
		if s["job"] != "api" {
			t.Fatalf("series with job=%q leaked past matcher", s["job"])
		}
		if s["__name__"] != "up" {
			t.Fatalf("missing/incorrect __name__: %+v", s)
		}
	}
}

func TestLabelValuesScopedByMatch(t *testing.T) {
	srv := fakeCH(t)
	defer srv.Close()
	h := newTestHandler(srv.URL)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/label/instance/values?match[]="+url.QueryEscape(`up{job="api"}`), nil)
	req.SetPathValue("name", "instance")
	w := httptest.NewRecorder()
	h.LabelValues(w, req)

	data := decodeAPI(t, w.Body.Bytes())
	var values []string
	if err := json.Unmarshal(data["data"], &values); err != nil {
		t.Fatalf("decode values: %v", err)
	}
	sort.Strings(values)
	want := []string{"h1", "h2"} // h3 is job=db, excluded
	if strings.Join(values, ",") != strings.Join(want, ",") {
		t.Fatalf("instance values = %v, want %v", values, want)
	}
}

func TestLabelsScopedByMatchIncludesName(t *testing.T) {
	srv := fakeCH(t)
	defer srv.Close()
	h := newTestHandler(srv.URL)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/labels?match[]="+url.QueryEscape(`up{job="api"}`), nil)
	w := httptest.NewRecorder()
	h.Labels(w, req)

	data := decodeAPI(t, w.Body.Bytes())
	var names []string
	if err := json.Unmarshal(data["data"], &names); err != nil {
		t.Fatalf("decode labels: %v", err)
	}
	got := strings.Join(sortedCopy(names), ",")
	want := "__name__,instance,job"
	if got != want {
		t.Fatalf("label names = %q, want %q", got, want)
	}
}

func TestMetadataPopulated(t *testing.T) {
	srv := fakeCH(t)
	defer srv.Close()
	h := newTestHandler(srv.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metadata", nil)
	w := httptest.NewRecorder()
	h.Metadata(w, req)

	data := decodeAPI(t, w.Body.Bytes())
	var md map[string][]metricMetadata
	if err := json.Unmarshal(data["data"], &md); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if len(md) == 0 {
		t.Fatal("metadata must not be empty")
	}
	if _, ok := md["up"]; !ok {
		t.Fatalf("expected metric 'up' in metadata, got keys %v", keysOf(md))
	}
}

func TestMetadataMetricFilter(t *testing.T) {
	srv := fakeCH(t)
	defer srv.Close()
	h := newTestHandler(srv.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metadata?metric=foo", nil)
	w := httptest.NewRecorder()
	h.Metadata(w, req)

	data := decodeAPI(t, w.Body.Bytes())
	var md map[string][]metricMetadata
	_ = json.Unmarshal(data["data"], &md)
	if len(md) != 1 || len(md["foo"]) != 1 {
		t.Fatalf("metric filter should return exactly {foo:[...]}, got %v", md)
	}
}

func TestChMatcherWhere(t *testing.T) {
	sel, err := parseSelector(`up{job="api",env!="prod",instance=~"h.*",zone!~"eu.*"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	where := chMatcherWhere(sel.All, "metric_name", "labels")
	for _, want := range []string{
		"metric_name = 'up'",
		"JSONExtractString(labels, 'job') = 'api'",
		"JSONExtractString(labels, 'env') != 'prod'",
		"match(JSONExtractString(labels, 'instance'), '^(?:h.*)$')",
		"NOT match(JSONExtractString(labels, 'zone'), '^(?:eu.*)$')",
	} {
		if !strings.Contains(where, want) {
			t.Fatalf("WHERE missing %q\n got: %s", want, where)
		}
	}
}

func TestChEscapeInMatcherValue(t *testing.T) {
	// A value containing a quote must not break out of the SQL literal.
	sel, _ := parseSelector(`up{job="a'b"}`)
	where := chMatcherWhere(sel.All, "metric_name", "labels")
	if !strings.Contains(where, `'a\'b'`) {
		t.Fatalf("quote not escaped in WHERE: %s", where)
	}
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

func keysOf(m map[string][]metricMetadata) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
