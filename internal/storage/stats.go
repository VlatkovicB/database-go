package storage

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const statsBuckets = 100
const statsMCVCount = 10

// MCVEntry is a most-common-value entry, mirroring pg_stats.most_common_vals/freqs.
type MCVEntry struct {
	Value interface{}
	Freq  float64 // fraction of non-null rows
}

// ColumnStats mirrors what PostgreSQL stores in pg_stats after ANALYZE.
type ColumnStats struct {
	NullFrac  float64       // fraction of rows that are NULL (0.0–1.0)
	NDistinct float64       // >0 = exact count; <0 = fraction of rows (like PG: -1 = all distinct)
	AvgWidth  int           // average byte width of non-null values
	Histogram []interface{} // equi-height bucket boundaries (up to statsBuckets+1 values)
	MCV       []MCVEntry    // top-N most common values by frequency
}

// TableStats mirrors what PostgreSQL stores in pg_stat_user_tables after ANALYZE.
type TableStats struct {
	NLiveTup int64
	NPages   int
	Columns  map[string]*ColumnStats
}

// FormatAnalyzeOutput returns a human-readable ANALYZE report for the frontend.
func (ts *TableStats) FormatAnalyzeOutput(tableName string, cols []Column) []string {
	var lines []string
	lines = append(lines, fmt.Sprintf("ANALYZE %s", tableName))
	lines = append(lines, fmt.Sprintf("  rows: %d  pages: %d", ts.NLiveTup, ts.NPages))
	lines = append(lines, "")
	for _, col := range cols {
		cs := ts.Columns[col.Name]
		if cs == nil {
			continue
		}
		nd := cs.NDistinct
		ndStr := fmt.Sprintf("%.0f", nd)
		if nd < 0 {
			ndStr = fmt.Sprintf("%.0f (all distinct)", nd*float64(-ts.NLiveTup))
		}
		nullPct := cs.NullFrac * 100
		line := fmt.Sprintf("  %-20s  n_distinct=%-6s  null_frac=%.1f%%  avg_width=%d",
			col.Name, ndStr, nullPct, cs.AvgWidth)
		lines = append(lines, line)
		if len(cs.MCV) > 0 {
			var parts []string
			for _, m := range cs.MCV {
				parts = append(parts, fmt.Sprintf("%v=%.1f%%", m.Value, m.Freq*100))
			}
			lines = append(lines, fmt.Sprintf("  %-20s  most_common: %s", "", strings.Join(parts, ", ")))
		}
		if len(cs.Histogram) > 0 {
			bounds := cs.Histogram
			if len(bounds) > 6 {
				bounds = []interface{}{bounds[0], bounds[1], bounds[2], "...", bounds[len(bounds)-3], bounds[len(bounds)-2], bounds[len(bounds)-1]}
			}
			var parts []string
			for _, b := range bounds {
				parts = append(parts, fmt.Sprintf("%v", b))
			}
			lines = append(lines, fmt.Sprintf("  %-20s  histogram: [%s]", "", strings.Join(parts, ", ")))
		}
	}
	return lines
}

// computeStats scans all rows in t and computes per-column statistics.
func computeStats(t *Table) *TableStats {
	nRows := int64(0)
	for _, pg := range t.Pages {
		nRows += int64(len(pg.Tuples))
	}

	ts := &TableStats{
		NLiveTup: nRows,
		NPages:   len(t.Pages),
		Columns:  make(map[string]*ColumnStats),
	}

	if nRows == 0 {
		for _, col := range t.Columns {
			ts.Columns[col.Name] = &ColumnStats{}
		}
		return ts
	}

	for _, col := range t.Columns {
		ts.Columns[col.Name] = computeColumnStats(t, col, nRows)
	}
	return ts
}

func computeColumnStats(t *Table, col Column, nRows int64) *ColumnStats {
	nullCount := int64(0)
	totalWidth := 0

	var nonNullVals []interface{}

	for _, pg := range t.Pages {
		for _, tup := range pg.Tuples {
			v, exists := tup.Data[col.Name]
			if !exists || v == nil {
				nullCount++
				continue
			}
			nonNullVals = append(nonNullVals, v)
			totalWidth += estimateValueWidth(v)
		}
	}

	cs := &ColumnStats{}
	cs.NullFrac = float64(nullCount) / float64(nRows)

	if len(nonNullVals) == 0 {
		return cs
	}

	cs.AvgWidth = totalWidth / len(nonNullVals)

	// Count distinct values.
	distinctSet := make(map[interface{}]int) // value → count
	for _, v := range nonNullVals {
		key := normalizeKey(v)
		distinctSet[key]++
	}
	nDistinct := float64(len(distinctSet))

	// PG convention: if nearly every row is distinct, store as negative fraction.
	// We use the same heuristic: if n_distinct > 20% of rows, store as -1.
	if nDistinct > 0.2*float64(nRows) {
		cs.NDistinct = -1.0 // all distinct
	} else {
		cs.NDistinct = nDistinct
	}

	// Most common values (top statsMCVCount by count).
	type kv struct {
		key   interface{}
		count int
	}
	var freqList []kv
	for k, cnt := range distinctSet {
		freqList = append(freqList, kv{k, cnt})
	}
	sort.Slice(freqList, func(i, j int) bool { return freqList[i].count > freqList[j].count })
	nonNullCount := float64(len(nonNullVals))
	for i := 0; i < statsMCVCount && i < len(freqList); i++ {
		freq := float64(freqList[i].count) / nonNullCount
		if freq < 0.01 {
			break // don't store rare values as MCV
		}
		cs.MCV = append(cs.MCV, MCVEntry{Value: freqList[i].key, Freq: freq})
	}

	// Equi-height histogram — only for orderable types (INT, FLOAT, TEXT).
	// Skip BOOLEAN (only 2 values — MCV covers it).
	if col.Type != TypeBoolean && nDistinct > 1 {
		sorted := sortedValues(nonNullVals, col.Type)
		if sorted != nil {
			// Remove MCV entries from histogram population (PG does this).
			mcvSet := make(map[interface{}]bool)
			for _, m := range cs.MCV {
				mcvSet[m.Value] = true
			}
			var histVals []interface{}
			for _, v := range sorted {
				if !mcvSet[normalizeKey(v)] {
					histVals = append(histVals, v)
				}
			}
			if len(histVals) > statsBuckets {
				// Pick statsBuckets+1 evenly-spaced boundaries.
				step := float64(len(histVals)-1) / float64(statsBuckets)
				for i := 0; i <= statsBuckets; i++ {
					idx := int(math.Round(float64(i) * step))
					if idx >= len(histVals) {
						idx = len(histVals) - 1
					}
					cs.Histogram = append(cs.Histogram, histVals[idx])
				}
			} else if len(histVals) > 1 {
				// Fewer values than buckets — use all unique boundary values.
				cs.Histogram = histVals
			}
		}
	}

	return cs
}

// normalizeKey returns a comparable map key for v.
// Needed because map[interface{}] uses Go equality — int vs int64 differ.
func normalizeKey(v interface{}) interface{} {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return float64(x)
	}
	return v
}

func estimateValueWidth(v interface{}) int {
	switch x := v.(type) {
	case int64:
		return 8
	case float64:
		return 8
	case bool:
		return 1
	case string:
		return len(x)
	}
	return 4
}

// sortedValues returns a sorted copy of vals for orderable column types.
func sortedValues(vals []interface{}, colType ColumnType) []interface{} {
	out := make([]interface{}, len(vals))
	copy(out, vals)

	switch colType {
	case TypeInt:
		sort.Slice(out, func(i, j int) bool {
			return toInt64(out[i]) < toInt64(out[j])
		})
	case TypeFloat:
		sort.Slice(out, func(i, j int) bool {
			return toFloat64(out[i]) < toFloat64(out[j])
		})
	case TypeText:
		sort.Slice(out, func(i, j int) bool {
			return fmt.Sprintf("%v", out[i]) < fmt.Sprintf("%v", out[j])
		})
	default:
		return nil
	}
	return out
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func toFloat64(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// HistogramSelectivity estimates the fraction of rows satisfying "col op val"
// using the equi-height histogram. Returns (selectivity, ok).
func (cs *ColumnStats) HistogramSelectivity(op string, val interface{}) (float64, bool) {
	if len(cs.Histogram) < 2 {
		return 0, false
	}
	n := len(cs.Histogram) - 1 // number of buckets

	// Find position in histogram via binary search.
	pos := histogramPosition(cs.Histogram, val)

	nonNull := 1.0 - cs.NullFrac

	switch op {
	case "<", "<=":
		// fraction of values below val
		sel := pos / float64(n)
		if op == "<=" {
			sel += 1.0 / float64(n) / 2.0 // add half a bucket for equality
		}
		if sel > 1.0 {
			sel = 1.0
		}
		return sel * nonNull, true
	case ">", ">=":
		sel := 1.0 - pos/float64(n)
		if op == ">=" {
			sel += 1.0 / float64(n) / 2.0
		}
		if sel > 1.0 {
			sel = 1.0
		}
		return sel * nonNull, true
	}
	return 0, false
}

// histogramPosition returns the fractional bucket position (0..n) of val.
func histogramPosition(hist []interface{}, val interface{}) float64 {
	n := len(hist)
	if n == 0 {
		return 0
	}
	// Try numeric comparison.
	fval, isNum := toFloat64Safe(val)
	if isNum {
		// Binary search for position.
		lo, hi := 0, n-1
		for lo < hi {
			mid := (lo + hi) / 2
			fmid, _ := toFloat64Safe(hist[mid])
			if fmid <= fval {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		// Interpolate within the bucket.
		if lo == 0 {
			return 0
		}
		if lo >= n {
			return float64(n - 1)
		}
		flo, _ := toFloat64Safe(hist[lo-1])
		fhi, _ := toFloat64Safe(hist[lo])
		if fhi == flo {
			return float64(lo - 1)
		}
		return float64(lo-1) + (fval-flo)/(fhi-flo)
	}
	// Text comparison.
	sval := fmt.Sprintf("%v", val)
	for i, h := range hist {
		if fmt.Sprintf("%v", h) >= sval {
			return float64(i)
		}
	}
	return float64(n - 1)
}

func toFloat64Safe(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	}
	return 0, false
}

// EqualitySelectivity estimates selectivity of "col = val".
// Checks MCV first, then falls back to 1/n_distinct.
func (cs *ColumnStats) EqualitySelectivity(val interface{}) float64 {
	key := normalizeKey(val)
	for _, m := range cs.MCV {
		if normalizeKey(m.Value) == key {
			return m.Freq * (1.0 - cs.NullFrac)
		}
	}
	nd := cs.NDistinct
	if nd < 0 {
		// PG convention: negative means fraction; but for us -1 means all distinct.
		// Approximate distinct count from NDistinct=-1 signal.
		// Caller should provide row count to scale; here we return a default fraction.
		return 0.005 // very selective when all values are distinct
	}
	if nd == 0 {
		return 0
	}
	mcvFrac := 0.0
	for _, m := range cs.MCV {
		mcvFrac += m.Freq
	}
	return (1.0 - mcvFrac) / nd * (1.0 - cs.NullFrac)
}
