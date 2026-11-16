package assettransfer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func readCSVRecords(text string) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1 // 每行列数可变；表头映射阶段统一处理
	r.LazyQuotes = false
	r.TrimLeadingSpace = false
	return r.ReadAll()
}

func parseNestedJSON(sheet string, line int, field, text string) (any, *RowError) {
	var v any
	if err := decodeStrictJSON(text, MaxJSONDepth, &v); err != nil {
		return nil, &RowError{Sheet: sheet, Line: line, Field: field, Reason: field + " JSON 结构无效"}
	}
	return v, nil
}
func parsePortsJSON(sheet string, line int, text string) ([]PortEntry, *RowError) {
	v, e := parseNestedJSON(sheet, line, "open_ports", text)
	if e != nil {
		return nil, e
	}
	out, err := jsonToPorts(line, v)
	if err != nil {
		return nil, &RowError{Sheet: sheet, Line: line, Field: "open_ports", Reason: err.Error()}
	}
	return out, nil
}
func parseAuthJSON(sheet string, line int, text string) ([]AuthEntry, *RowError) {
	v, e := parseNestedJSON(sheet, line, "auth", text)
	if e != nil {
		return nil, e
	}
	out, err := jsonToAuth(line, v)
	if err != nil {
		return nil, &RowError{Sheet: sheet, Line: line, Field: "auth", Reason: err.Error()}
	}
	return out, nil
}
func parseParamsJSON(sheet string, line int, text string) ([]ParamEntry, *RowError) {
	v, e := parseNestedJSON(sheet, line, "params", text)
	if e != nil {
		return nil, e
	}
	out, err := jsonToParams(line, v)
	if err != nil {
		return nil, &RowError{Sheet: sheet, Line: line, Field: "params", Reason: err.Error()}
	}
	return out, nil
}

func toInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x || x >= math.Pow(2, strconv.IntSize-1) || x < -math.Pow(2, strconv.IntSize-1) {
			return 0, fmt.Errorf("must be an integer in range")
		}
		return int(x), nil
	case int:
		return x, nil
	case json.Number:
		n, err := x.Int64()
		if err != nil || int64(int(n)) != n {
			return 0, fmt.Errorf("integer out of range")
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("not a number")
}

// marshalCompact renders stable JSON for diffs/fingerprints.
func marshalCompact(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSpace(buf.String())
}
