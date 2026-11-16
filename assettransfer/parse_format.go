package assettransfer

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// ParseXLSX parses an .xlsx upload. 解压量与 ZIP 条目数封顶（方案 §13），
// 不执行公式：单元格按原文读取，公式结果列也不参与（模板不产生公式）。
func ParseXLSX(data []byte) (*ParseResult, error) {
	if len(data) > MaxUploadBytes {
		return nil, parseErr(413, "文件超过上传大小上限")
	}
	if err := checkXLSXZipLimits(data); err != nil {
		return nil, err
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, parseErr(422, "XLSX 文件无效: %v", err)
	}
	defer f.Close()

	res := &ParseResult{Format: "xlsx"}
	sheetNames := f.GetSheetList()
	if len(sheetNames) == 0 {
		return nil, parseErr(422, "XLSX 不含任何工作表")
	}
	known := 0
	for _, name := range sheetNames {
		contract := sheetByName(name)
		if contract == nil {
			if name == "填写说明" {
				continue
			}
			res.Warnings = append(res.Warnings, &RowWarning{Sheet: name, Detail: "工作表「" + name + "」未采用：不是模板合同表名"})
			continue
		}
		known++
		rowsIter, err := f.Rows(name)
		if err != nil {
			return nil, parseErr(422, "读取工作表 %s 失败: %v", name, err)
		}
		parseSheetRows(res, contract, rowsIter, func() error { return rowsIter.Close() })
	}
	if known == 0 {
		return nil, parseErr(422, "XLSX 中没有任何合同工作表（需要：填写说明 + %s 等）", sheets[0].name)
	}
	return res, nil
}

func parseSheetRows(res *ParseResult, contract *sheetContract, iter *excelize.Rows, close func() error) {
	defer close()
	line := 0
	var colMap map[int]string
	for iter.Next() {
		line++
		cells, err := iter.Columns()
		if err != nil {
			res.Errors = append(res.Errors, &RowError{Sheet: contract.name, Line: line, Reason: "读取行失败: " + err.Error()})
			continue
		}
		if err := checkCellSizes(cells); err != nil {
			res.Errors = append(res.Errors, &RowError{Sheet: contract.name, Line: line, Reason: err.Error()})
			continue
		}
		if line == 1 {
			var warnings []string
			var herr *RowError
			colMap, warnings, herr = buildHeaderMap(contract.name, 1, cells, contract.cols)
			if herr != nil {
				res.Errors = append(res.Errors, herr)
				return
			}
			for _, w := range warnings {
				res.Warnings = append(res.Warnings, &RowWarning{Sheet: contract.name, Detail: w})
			}
			continue
		}
		if isAllEmpty(cells) {
			continue
		}
		if line == 2 && isDescRow(contract, cells, colMap) {
			continue // 模板说明行
		}
		if err := checkCellSizes(cells); err != nil {
			res.Errors = append(res.Errors, &RowError{Sheet: contract.name, Line: line, Reason: err.Error()})
			continue
		}
		row, rerr := rowFromCells(contract.name, line, cells, colMap, contract.cols, nil, false)
		if rerr != nil {
			res.Errors = append(res.Errors, rerr)
			continue
		}
		row.Type = contract.typ
		if contract.force != "" {
			row.ServiceType = contract.force
		}
		if err := finalizeRow(row); err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		if len(res.Rows)+len(res.Errors) >= MaxRows {
			res.Errors = append(res.Errors, &RowError{Sheet: contract.name, Line: line, Reason: "数据行超过单批上限"})
			return
		}
		res.Rows = append(res.Rows, row)
	}
}

// isDescRow 判断第 2 行是否为模板说明行：每个合同列的值与该列 desc 完全一致。
func isDescRow(contract *sheetContract, cells []string, colMap map[int]string) bool {
	if len(colMap) == 0 {
		return false
	}
	matched := 0
	for idx, key := range colMap {
		for _, c := range contract.cols {
			if c.key == key {
				if idx < len(cells) && strings.TrimSpace(cells[idx]) == columnDescription(c) {
					matched++
				}
				break
			}
		}
	}
	return matched == len(colMap)
}

func checkCellSizes(cells []string) error {
	for _, c := range cells {
		if len(c) > MaxCellBytes {
			return fmt.Errorf("单元格内容超过 %d 字节上限", MaxCellBytes)
		}
	}
	return nil
}

// checkXLSXZipLimits walks the zip central directory equivalents: 每个条目解压
// 上限与总解压量、条目总数（方案 §13）。
func checkXLSXZipLimits(data []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return parseErr(422, "XLSX (ZIP) 结构无效: %v", err)
	}
	if len(zr.File) > MaxXLSXEntries {
		return parseErr(413, "XLSX ZIP 条目数 %d 超过上限 %d", len(zr.File), MaxXLSXEntries)
	}
	total := uint64(0)
	for _, zf := range zr.File {
		total += zf.UncompressedSize64
		if zf.UncompressedSize64 > uint64(MaxXLSXZipBytes) || total > uint64(MaxXLSXZipBytes) {
			return parseErr(413, "XLSX 解压总量超过 %d MiB 上限", MaxXLSXZipBytes>>20)
		}
	}
	return nil
}

// =====================================================================
// ARTEX JSON（方案 §6.5）
// =====================================================================

type jsonPackage struct {
	Format        string            `json:"format"`
	SchemaVersion int               `json:"schema_version"`
	ExportedAt    string            `json:"exported_at"`
	Source        jsonSource        `json:"source"`
	ExportPolicy  jsonPolicy        `json:"export_policy"`
	Assets        []json.RawMessage `json:"assets"`
}

// ParseJSON parses an ARTEX asset JSON package. 未知 format/版本/结构整体拒绝
// （不静默丢字段后声称完整）。
func ParseJSON(data []byte) (*ParseResult, error) {
	if len(data) > MaxUploadBytes {
		return nil, parseErr(413, "文件超过上传大小上限")
	}
	var pkg jsonPackage
	if err := decodeStrictJSON(string(data), MaxJSONDepth, &pkg); err != nil {
		return nil, parseErr(422, "JSON 解析失败: %v", err)
	}
	if pkg.Format != FormatName {
		return nil, parseErr(422, "format 必须是 %q，收到 %q", FormatName, pkg.Format)
	}
	if pkg.SchemaVersion != SchemaVersion {
		return nil, parseErr(422, "schema_version %d 不被支持（当前支持 %d）", pkg.SchemaVersion, SchemaVersion)
	}
	if pkg.Assets == nil {
		return nil, parseErr(422, "assets 必须是数组")
	}
	res := &ParseResult{Format: "json"}
	if len(pkg.Assets) > MaxRows {
		return nil, parseErr(413, "数据行超过单批上限 %d", MaxRows)
	}
	for i, raw := range pkg.Assets {
		line := i + 1
		row, err := rowFromJSON(line, raw)
		if err != nil {
			res.Errors = append(res.Errors, &RowError{Sheet: "JSON", Line: line, Reason: err.Error()})
			continue
		}
		if err := finalizeRow(row); err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// jsonFieldWhitelist: 每类资产允许的字段键（方案 §6.6）。source_ref 与 note 通用。
func rowFromJSON(line int, raw json.RawMessage) (*Row, error) {
	var m map[string]any
	if err := decodeStrictJSON(string(raw), MaxJSONDepth, &m); err != nil {
		return nil, fmt.Errorf("资产[%d] JSON 无效: %v", line, err)
	}
	// Preserve exact integer lexemes instead of rounding through float64.
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawFields); err != nil {
		return nil, err
	}
	for _, key := range []string{"status_code", "content_length", "port"} {
		if token, exists := rawFields[key]; exists {
			var number json.Number
			if err := json.Unmarshal(token, &number); err != nil || string(token) == "null" || len(token) == 0 || token[0] == '"' {
				return nil, fmt.Errorf("field %s must be an integer", key)
			}
			m[key] = number
		}
	}
	if err := checkJSONCellSizes(m); err != nil {
		return nil, err
	}
	typRaw, _ := m["type"].(string)
	typ, ok := typeAliases[strings.ToLower(strings.TrimSpace(typRaw))]
	if !ok {
		return nil, fmt.Errorf("资产[%d] 类型 %q 不被识别", line, typRaw)
	}
	r := &Row{Sheet: "JSON", Line: line, Type: typ}
	known := map[string]bool{"type": true, "note": true, "source_ref": true, "extra": true}
	switch typ {
	case "root_domain":
		for _, k := range []string{"domain", "icp"} {
			known[k] = true
		}
	case "ip":
		for _, k := range []string{"ip", "bound_domains", "open_ports"} {
			known[k] = true
		}
	case "subdomain":
		for _, k := range []string{"domain", "record_type", "record_value", "icp"} {
			known[k] = true
		}
	case "app":
		for _, k := range []string{"app_name", "bundle_id", "category", "app_description", "app_icp", "url"} {
			known[k] = true
		}
	case "service":
		for _, k := range []string{"url", "ip", "status_code", "content_length", "page_title", "technologies", "favicon_mmh3", "auth", "domain", "port", "service_name"} {
			known[k] = true
		}
	case "endpoint":
		for _, k := range []string{"url", "method", "ip", "params"} {
			known[k] = true
		}
	}
	var unknown []string
	for k := range m {
		if m[k] == nil {
			return nil, fmt.Errorf("field %s cannot be null", k)
		}
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		// 球未知字段按行拒绝，排序保证信息稳定
		sortStrings(unknown)
		return nil, fmt.Errorf("资产[%d] 含未知字段（该类型不支持）: %s", line, strings.Join(unknown, ","))
	}

	for _, k := range []string{"type", "note", "domain", "ip", "icp", "record_type", "bundle_id", "app_name", "category", "app_description", "app_icp", "url", "page_title", "favicon_mmh3", "service_name", "method"} {
		if v, exists := m[k]; exists {
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("field %s must be a string", k)
			}
		}
	}
	str := func(k string) string { v, _ := m[k].(string); return v }
	if v := str("domain"); v != "" {
		r.Domain = parseDomain(v)
	}
	if v := str("ip"); v != "" {
		ip, ipErr := parseIPField("JSON", line, "ip", v)
		if ipErr != nil {
			return nil, fmt.Errorf("资产[%d] %s", line, ipErr.Reason)
		}
		r.IP = ip
	}
	r.ICP = str("icp")
	r.RecordType = str("record_type")
	r.BundleID = str("bundle_id")
	r.AppName = str("app_name")
	r.Category = str("category")
	r.AppDescription = str("app_description")
	r.AppICP = str("app_icp")
	r.URL = strings.TrimSpace(str("url"))
	r.PageTitle = str("page_title")
	r.FaviconMMH3 = str("favicon_mmh3")
	r.ServiceName = strings.ToLower(strings.TrimSpace(str("service_name")))
	r.Method = str("method")
	r.Note = str("note")

	for _, k := range []string{"record_value", "bound_domains", "technologies"} {
		if v, ok := m[k]; ok {
			list, err := toStringSlice(v, k)
			if err != nil {
				return nil, err
			}
			switch k {
			case "record_value":
				r.RecordValue = list
			case "bound_domains":
				r.BoundDomains = list
			case "technologies":
				r.Technologies = list
			}
		}
	}
	if v, ok := m["status_code"]; ok && v != nil {
		n, err := toInt(v)
		if err != nil {
			return nil, fmt.Errorf("资产[%d] status_code 无效", line)
		}
		r.StatusCode = &n
	}
	if v, ok := m["content_length"]; ok && v != nil {
		cl, err := strconv.ParseInt(stringValueNumber(v), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("资产[%d] content_length 必须是整数", line)
		}
		r.ContentLength = &cl
	}
	if v, ok := m["port"]; ok && v != nil {
		n, err := toInt(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("资产[%d] port 无效", line)
		}
		r.Port = n
	}
	if v, ok := m["open_ports"]; ok && v != nil {
		ps, err := jsonToPorts(line, v)
		if err != nil {
			return nil, err
		}
		r.OpenPorts = ps
	}
	if v, ok := m["auth"]; ok && v != nil {
		auth, err := jsonToAuth(line, v)
		if err != nil {
			return nil, err
		}
		r.Auth = auth
	}
	if v, ok := m["params"]; ok && v != nil {
		ps, err := jsonToParams(line, v)
		if err != nil {
			return nil, err
		}
		r.Params = ps
	}
	if v, ok := m["extra"]; ok && v != nil {
		extra, ok2 := v.(map[string]any)
		if !ok2 {
			return nil, fmt.Errorf("资产[%d] extra 必须是对象", line)
		}
		r.Extra = extra
	}
	if v, ok := m["source_ref"]; ok && v != nil {
		b := rawFields["source_ref"]
		var sr SourceRef
		if err := decodeStrictJSON(string(b), MaxJSONDepth, &sr); err != nil {
			return nil, fmt.Errorf("资产[%d] source_ref 结构无效", line)
		}
		r.SourceRef = &sr
	}
	// service 分类：URL 优先 http
	if typ == "service" {
		if r.URL != "" {
			r.ServiceType = "http"
		} else {
			r.ServiceType = "other"
		}
	}
	return r, nil
}

func toStringSlice(v any, field string) ([]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("field %s must be an array of strings", field)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("field %s must contain only strings", field)
		}
		out = append(out, text)
	}
	return dedupeNonEmpty(out), nil
}

func stringValueNumber(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return string(x)
	}
	return ""
}

func jsonToPorts(line int, v any) ([]PortEntry, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("资产[%d] open_ports 必须是数组", line)
	}
	out := make([]PortEntry, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("资产[%d] open_ports 项必须是对象", line)
		}
		if err := validateObjectFields(m, map[string]bool{"port": true, "service": false}); err != nil {
			return nil, err
		}
		port, err := toInt(m["port"])
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("资产[%d] open_ports 项 port 无效", line)
		}
		svc, _ := m["service"].(string)
		if len(m) > 2 {
			return nil, fmt.Errorf("资产[%d] open_ports 项含未知键", line)
		}
		out = append(out, PortEntry{Port: port, Service: svc})
	}
	return out, nil
}

func jsonToAuth(line int, v any) ([]AuthEntry, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("资产[%d] auth 必须是数组", line)
	}
	out := make([]AuthEntry, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("资产[%d] auth 项必须是对象", line)
		}
		if err := validateObjectFields(m, map[string]bool{"type": false, "username": false, "password": false, "token": false, "description": false}); err != nil {
			return nil, err
		}
		e := AuthEntry{}
		e.Type, _ = m["type"].(string)
		e.Username, _ = m["username"].(string)
		e.Password, _ = m["password"].(string)
		e.Token, _ = m["token"].(string)
		e.Description, _ = m["description"].(string)
		out = append(out, e)
	}
	return out, nil
}

func jsonToParams(line int, v any) ([]ParamEntry, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("资产[%d] params 必须是数组", line)
	}
	out := make([]ParamEntry, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("资产[%d] params 项必须是对象", line)
		}
		if err := validateObjectFields(m, map[string]bool{"location": false, "name": false, "value": false, "type": false}); err != nil {
			return nil, err
		}
		name, _ := m["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("资产[%d] params 项缺少 name", line)
		}
		p := ParamEntry{}
		p.Location, _ = m["location"].(string)
		p.Name = name
		p.Value, _ = m["value"].(string)
		p.Type, _ = m["type"].(string)
		out = append(out, p)
	}
	return out, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var _ io.Reader = (*bytes.Reader)(nil)
