package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/PromClick/PromClick/fingerprint"
)

// seriesLimit bounds the number of series returned by /api/v1/series and the
// number scanned to answer scoped /labels and /label/{name}/values.
const seriesLimit = 10000

// Series handles /api/v1/series — returns label sets matching match[] selectors.
func (h *Handler) Series(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	matchers := r.Form["match[]"]
	if len(matchers) == 0 {
		writeJSON(w, APIResponse{Status: "success", Data: []interface{}{}})
		return
	}

	sets, err := h.matchedSeries(r.Context(), matchers, seriesLimit)
	if err != nil {
		// Prometheus returns 200 with an empty result for unmatched/parse-empty
		// selectors; surface parse errors as bad_data.
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if sets == nil {
		sets = []map[string]string{}
	}
	writeJSON(w, APIResponse{Status: "success", Data: sets})
}

// matchedSeries resolves the distinct label sets (each including __name__)
// matching any of the given match[] selectors, applying every label matcher.
// It prefers the in-memory label cache when a selector carries a concrete
// metric name, falling back to a scoped ClickHouse query otherwise.
func (h *Handler) matchedSeries(ctx context.Context, rawMatchers []string, limit int) ([]map[string]string, error) {
	var out []map[string]string
	seen := make(map[uint64]struct{})

	add := func(set map[string]string) bool {
		fp := fingerprint.Compute(set)
		if _, dup := seen[fp]; dup {
			return true
		}
		seen[fp] = struct{}{}
		out = append(out, set)
		return len(out) < limit
	}

	useCache := h.Pool != nil && h.Pool.LabelCache != nil && h.Pool.LabelCache.IsLoaded()

	for _, raw := range rawMatchers {
		sel, err := parseSelector(raw)
		if err != nil {
			return nil, err
		}

		// Cache fast-path: needs a concrete metric name (regex/negated name
		// matchers have no metric index to scan).
		if useCache && sel.MetricName != "" {
			if fps, ok := h.Pool.LabelCache.GetFingerprints(sel.MetricName, sel.Matchers); ok {
				for _, fp := range fps {
					lbls, hit := h.Pool.LabelCache.GetLabels(fp)
					if !hit {
						continue
					}
					set := make(map[string]string, len(lbls)+1)
					set["__name__"] = sel.MetricName
					for k, v := range lbls {
						set[k] = v
					}
					if !add(set) {
						return out, nil
					}
				}
				continue
			}
		}

		// ClickHouse fallback: apply all matchers in SQL.
		sets, err := h.Meta.SeriesMatch(ctx, sel,
			h.PromCfg.Schema.TimeSeriesTable,
			h.PromCfg.Schema.Columns.MetricName,
			h.PromCfg.Schema.Columns.Labels,
			limit)
		if err != nil {
			slog.Warn("series: clickhouse fallback failed", "error", err)
			return nil, nil
		}
		for _, set := range sets {
			if !add(set) {
				return out, nil
			}
		}
	}
	return out, nil
}
