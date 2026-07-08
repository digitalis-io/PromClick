package handlers

import (
	"net/http"
	"sort"
)

// Labels handles /api/v1/labels. When match[] selectors are supplied, the label
// names are scoped to the matching series; otherwise all label names are
// returned. start/end are accepted for Prometheus compatibility but not used to
// prune series (the label cache has no time dimension).
func (h *Handler) Labels(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	if matchers := r.Form["match[]"]; len(matchers) > 0 {
		sets, err := h.matchedSeries(r.Context(), matchers, seriesLimit)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		nameSet := map[string]struct{}{"__name__": {}}
		for _, s := range sets {
			for k := range s {
				nameSet[k] = struct{}{}
			}
		}
		names := make([]string, 0, len(nameSet))
		for k := range nameSet {
			names = append(names, k)
		}
		sort.Strings(names)
		writeJSON(w, APIResponse{Status: "success", Data: names})
		return
	}

	labels, err := h.Meta.Labels(
		r.Context(),
		h.PromCfg.Schema.TimeSeriesTable,
		h.PromCfg.Schema.Columns.Labels,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err)
		return
	}
	writeJSON(w, APIResponse{
		Status: "success",
		Data:   labels,
	})
}

// LabelValues handles /api/v1/label/{name}/values. When match[] selectors are
// supplied, values are scoped to the matching series; otherwise all values for
// the label are returned. start/end are accepted but not used to prune series.
func (h *Handler) LabelValues(w http.ResponseWriter, r *http.Request) {
	labelName := r.PathValue("name")
	if labelName == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing label name")
		return
	}
	_ = r.ParseForm()

	if matchers := r.Form["match[]"]; len(matchers) > 0 {
		sets, err := h.matchedSeries(r.Context(), matchers, seriesLimit)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		valueSet := make(map[string]struct{})
		for _, s := range sets {
			if v, ok := s[labelName]; ok && v != "" {
				valueSet[v] = struct{}{}
			}
		}
		values := make([]string, 0, len(valueSet))
		for v := range valueSet {
			values = append(values, v)
		}
		sort.Strings(values)
		writeJSON(w, APIResponse{Status: "success", Data: values})
		return
	}

	values, err := h.Meta.LabelValues(
		r.Context(),
		labelName,
		h.PromCfg.Schema.TimeSeriesTable,
		h.PromCfg.Schema.Columns.MetricName,
		h.PromCfg.Schema.Columns.Labels,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err)
		return
	}
	if values == nil {
		values = []string{}
	}
	writeJSON(w, APIResponse{
		Status: "success",
		Data:   values,
	})
}

// TSDBStatus handles /api/v1/status/tsdb.
func (h *Handler) TSDBStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.Meta.TSDBStatus(
		r.Context(),
		h.PromCfg.Schema.TimeSeriesTable,
		h.PromCfg.Schema.Columns.MetricName,
		h.PromCfg.Schema.Columns.Labels,
		h.PromCfg.Schema.SamplesTable,
		h.PromCfg.Schema.Columns.Timestamp,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err)
		return
	}
	writeJSON(w, APIResponse{
		Status: "success",
		Data:   status,
	})
}
