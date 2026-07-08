package handlers

import (
	"net/http"
	"strconv"
)

// metricMetadata is one entry of the Prometheus /api/v1/metadata response.
// PromClick does not record HELP/TYPE/UNIT (remote_write does not carry it in
// the sample stream), so type is reported as "unknown" — enough to populate
// Grafana's metric browser.
type metricMetadata struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

// Metadata handles /api/v1/metadata. It returns one "unknown"-typed entry per
// metric name so Grafana's metric browser and autocomplete are populated.
// Honours the optional `metric` and `limit` query parameters.
func (h *Handler) Metadata(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	metric := formOrQuery(r, "metric")
	limit := -1
	if s := formOrQuery(r, "limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}

	out := make(map[string][]metricMetadata)
	entry := []metricMetadata{{Type: "unknown", Help: "", Unit: ""}}

	if metric != "" {
		out[metric] = entry
		writeJSON(w, APIResponse{Status: "success", Data: out})
		return
	}

	if limit == 0 {
		writeJSON(w, APIResponse{Status: "success", Data: out})
		return
	}

	names, err := h.Meta.LabelValues(
		r.Context(),
		"__name__",
		h.PromCfg.Schema.TimeSeriesTable,
		h.PromCfg.Schema.Columns.MetricName,
		h.PromCfg.Schema.Columns.Labels,
	)
	if err != nil {
		// Degrade gracefully: an empty (but well-formed) metadata map.
		writeJSON(w, APIResponse{Status: "success", Data: out})
		return
	}
	for _, n := range names {
		if limit > 0 && len(out) >= limit {
			break
		}
		out[n] = entry
	}
	writeJSON(w, APIResponse{Status: "success", Data: out})
}
