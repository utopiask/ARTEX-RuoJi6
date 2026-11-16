package assettransfer

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ParseTXT parses pasted/plain-text lines (方案 §6.4)。每行独立分类，全部本地
// 判定：不查 DNS、不请求 URL。CIDR 明确报不支持；无法识别的行报错而不是
// 当作关键词。
func ParseTXT(text string) *ParseResult {
	res := &ParseResult{Format: "txt"}
	if len(text) > MaxUploadBytes {
		res.Errors = append(res.Errors, &RowError{Sheet: "粘贴", Reason: "文件超过上传大小上限"})
		return res
	}
	lineNo := 0
	for _, raw := range strings.Split(text, "\n") {
		lineNo++
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(res.Rows)+len(res.Errors) >= MaxRows {
			res.Errors = append(res.Errors, &RowError{Sheet: "粘贴", Line: lineNo, Reason: "数据行超过单批上限"})
			return res
		}
		if len(raw) > MaxCellBytes {
			res.Errors = append(res.Errors, &RowError{Sheet: "粘贴", Line: lineNo, Reason: "单元格内容超过字节上限"})
			continue
		}
		row := classifyTXTLine(line, lineNo)
		if err := finalizeRow(row); err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

var reMethod = regexp.MustCompile(`^[A-Za-z]{3,10}$`)

// classifyTXTLine implements the per-line table in 方案 §6.4。
func classifyTXTLine(line string, lineNo int) *Row {
	// "METHOD URL" → endpoint（分别校验方法和 URL）
	if fields := strings.Fields(line); len(fields) == 2 && reMethod.MatchString(fields[0]) {
		m := strings.ToUpper(fields[0])
		if knownMethods[m] && (strings.HasPrefix(strings.ToLower(fields[1]), "http://") || strings.HasPrefix(strings.ToLower(fields[1]), "https://")) {
			return &Row{Sheet: "粘贴", Line: lineNo, Type: "endpoint", Method: m, URL: strings.TrimSpace(fields[1])}
		}
		// 形似 METHOD URL 但方法/URL 不合法 → 端点行错误，不落到其它分类
		r := &Row{Sheet: "粘贴", Line: lineNo, Type: "endpoint", Method: fields[0], URL: fields[1]}
		return r
	}

	// URL（含路径与查询，原样保留）
	if strings.HasPrefix(strings.ToLower(line), "http://") || strings.HasPrefix(strings.ToLower(line), "https://") {
		return &Row{Sheet: "粘贴", Line: lineNo, Type: "service", ServiceType: "http", URL: strings.TrimSpace(line)}
	}

	// CIDR：明确不支持，不展开、不登记范围
	if _, _, err := net.ParseCIDR(line); err == nil {
		return &Row{Sheet: "粘贴", Line: lineNo, Type: "cidr",
			Domain: line} // 借 Domain 放原始串；finalizeRow 会因未知类型报错
	}
	// 形如 1.2.3.4:8080 / https://… 的「端口/协议」歧义串：交给后续判定或报错
	if strings.Contains(line, " ") {
		return &Row{Sheet: "粘贴", Line: lineNo, Type: "", Domain: line}
	}

	// 裸 IP
	if net.ParseIP(line) != nil {
		return &Row{Sheet: "粘贴", Line: lineNo, Type: "ip", IP: line}
	}

	// host:port（非 URL）→ 明确提示用模板
	if _, port, ok := splitHostPort(line); ok && port != "" {
		return &Row{Sheet: "粘贴", Line: lineNo, Type: "host_port", Domain: line}
	}

	// 域名：注册域名 vs 子域名（本地计算根域名，不查 DNS）
	host := parseDomain(line)
	if host != "" && strings.Contains(host, ".") && !strings.ContainsAny(host, "/\\?#@") {
		if u, err := url.Parse("http://" + host + "/"); err == nil && u.Hostname() != "" {
			root, isApex := RootDomainLocal(host)
			if isApex {
				return &Row{Sheet: "粘贴", Line: lineNo, Type: "root_domain", Domain: host, RootDomain: root}
			}
			return &Row{Sheet: "粘贴", Line: lineNo, Type: "subdomain", Domain: host, RootDomain: root}
		}
	}

	// 其余（含无点内部名、协议/应用字符串）：行错误，提示使用模板
	return &Row{Sheet: "粘贴", Line: lineNo, Type: "", Domain: line}
}

func splitHostPort(s string) (string, string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	host, port := s[:i], s[i+1:]
	if strings.Contains(host, ":") { // IPv6 字面量
		return "", "", false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return host, port, true
}

// ParseCSVFromText parses CSV text (RFC 4180 via encoding/csv, UTF-8 可选 BOM，
// 多行单元格由 csv.Reader 处理，不简单切字符串)。escapedCols 仅对带
// _artex_csv_text 标记的列移除一层转义（方案 §6.3）。
func ParseCSVFromText(text string) *ParseResult {
	res := &ParseResult{Format: "csv"}
	text = strings.TrimPrefix(text, "\xEF\xBB\xBF")
	if len(text) > MaxUploadBytes {
		res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Reason: "文件超过上传大小上限"})
		return res
	}
	rows, err := readCSVRecords(text)
	if err != nil {
		res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Line: 1, Reason: "CSV 解析失败: " + err.Error()})
		return res
	}
	if len(text) > MaxUploadBytes {
		res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Reason: "文件超过上传大小上限"})
		return res
	}
	if len(rows) == 0 {
		res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Line: 1, Reason: "CSV 为空（缺少表头）"})
		return res
	}
	allCols := append([]colDef{csvTypeCol}, csvUnionColumns()...)
	header := rows[0]
	if err := checkCellSizes(header); err != nil {
		res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Line: 1, Reason: err.Error()})
		return res
	}
	colMap, warnings, herr := buildHeaderMap("CSV", 1, header, allCols)
	if herr != nil {
		res.Errors = append(res.Errors, herr)
		return res
	}
	for _, w := range warnings {
		res.Warnings = append(res.Warnings, &RowWarning{Sheet: "CSV", Detail: w})
	}
	if _, ok := colOf(colMap, "type"); !ok {
		res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Line: 1, Field: "type", Reason: "CSV 表头缺少资产类型列"})
		return res
	}
	// 导出元数据列：_artex_schema 存在时按 _artex_csv_text 列表移除一层转义
	escapedSet := csvEscapedColumns(header, rows)
	for i, rec := range rows[1:] {
		lineNo := i + 2
		if isAllEmpty(rec) {
			continue
		}
		logicalCells := append([]string(nil), rec...)
		for idx, key := range colMap {
			if idx < len(logicalCells) && escapedSet[key] && strings.HasPrefix(logicalCells[idx], "'") {
				logicalCells[idx] = logicalCells[idx][1:]
			}
		}
		if err := checkCellSizes(logicalCells); err != nil {
			res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Line: lineNo, Reason: err.Error()})
			continue
		}
		if i == 0 && isCSVMetaRow(header, rec) {
			continue // 导出元数据行（_artex_schema 标记）
		}
		if len(res.Rows)+len(res.Errors) >= MaxRows {
			res.Errors = append(res.Errors, &RowError{Sheet: "CSV", Line: lineNo, Reason: "数据行超过单批上限"})
			return res
		}
		row, rerr := rowFromCells("CSV", lineNo, rec, colMap, allCols, escapedSet, true)
		if rerr != nil {
			res.Errors = append(res.Errors, rerr)
			continue
		}
		if err := finalizeRow(row); err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

// csvEscapedColumns reads the optional metadata columns. 仅当 _artex_schema 等于
// 本格式标记时，_artex_csv_text 列出的列名才获得一层转义剥离；普通 CSV 原样。
func csvEscapedColumns(header []string, rows [][]string) map[string]bool {
	schemaIdx, textIdx := -1, -1
	for i, h := range header {
		switch strings.TrimSpace(h) {
		case "_artex_schema":
			schemaIdx = i
		case "_artex_csv_text":
			textIdx = i
		}
	}
	if schemaIdx < 0 || textIdx < 0 || len(rows) < 2 {
		return nil
	}
	rec := rows[1]
	if schemaIdx >= len(rec) || textIdx >= len(rec) {
		return nil
	}
	if !isCSVMetaRow(header, rec) || strings.TrimSpace(rec[schemaIdx]) != CSVSchemaMarker {
		return nil
	}
	set := map[string]bool{}
	for _, name := range strings.Split(rec[textIdx], ";") {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	return set
}

// isCSVMetaRow 识别导出写入的元数据行：_artex_schema 列有值且其余业务列为空。
func isCSVMetaRow(header []string, rec []string) bool {
	marker := false
	for i, v := range rec {
		h := ""
		if i < len(header) {
			h = strings.TrimSpace(header[i])
		}
		switch h {
		case "_artex_schema":
			marker = strings.TrimSpace(v) == CSVSchemaMarker
		case "_artex_csv_text":
		default:
			if strings.TrimSpace(v) != "" {
				return false
			}
		}
	}
	return marker
}

func colOf(colMap map[int]string, key string) (int, bool) {
	for idx, k := range colMap {
		if k == key {
			return idx, true
		}
	}
	return -1, false
}

func isAllEmpty(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// csvUnionColumns builds the unified per-type column list (去掉 type 本身)。
func csvUnionColumns() []colDef {
	seen := map[string]bool{}
	var out []colDef
	for _, sh := range sheets {
		for _, c := range sh.cols {
			if !seen[c.key] {
				seen[c.key] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// rowFromCells converts one record to a Row given a header→key map. csvMode
// adds type resolution from the type column and service auto-classification.
func rowFromCells(sheet string, line int, rec []string, colMap map[int]string, contract []colDef, escapedCols map[string]bool, csvMode bool) (*Row, *RowError) {
	r := &Row{Sheet: sheet, Line: line}
	get := func(key string) (string, bool) {
		idx, ok := colOf(colMap, key)
		if !ok || idx >= len(rec) {
			return "", false
		}
		v := rec[idx]
		// 本格式导出转义：只移除一层明确标记的前导单引号
		if escapedCols != nil && escapedCols[key] && strings.HasPrefix(v, "'") {
			v = v[1:]
		}
		return v, true
	}

	typRaw, _ := get("type")
	if csvMode {
		t := strings.ToLower(strings.TrimSpace(typRaw))
		mapped, ok := typeAliases[t]
		if !ok {
			return nil, &RowError{Sheet: sheet, Line: line, Field: "type", Reason: fmt.Sprintf("资产类型 %q 不被识别", truncateForReason(typRaw))}
		}
		r.Type = mapped
	}

	var cellErr *RowError
	fail := func(err *RowError) {
		if cellErr == nil {
			cellErr = err
		}
	}

	if v, ok := get("domain"); ok && strings.TrimSpace(v) != "" {
		r.Domain = parseDomain(v)
	}
	if v, ok := get("ip"); ok {
		ip, err := parseIPField(sheet, line, "ip", v)
		if err != nil {
			fail(err)
		} else {
			r.IP = ip
		}
	}
	if v, ok := get("icp"); ok {
		r.ICP = strings.TrimSpace(v)
	}
	if v, ok := get("record_type"); ok {
		r.RecordType = strings.TrimSpace(v)
	}
	if v, ok := get("record_value"); ok {
		items, err := parseListField(sheet, line, "record_value", v)
		if err != nil {
			fail(err)
		} else {
			r.RecordValue = items
		}
	}
	if v, ok := get("bound_domains"); ok {
		items, err := parseListField(sheet, line, "bound_domains", v)
		if err != nil {
			fail(err)
		} else {
			r.BoundDomains = items
		}
	}
	if v, ok := get("open_ports"); ok && strings.TrimSpace(v) != "" {
		ps, err := parsePortsJSON(sheet, line, v)
		if err != nil {
			fail(err)
		} else {
			r.OpenPorts = ps
		}
	}
	if v, ok := get("bundle_id"); ok {
		r.BundleID = strings.TrimSpace(v)
	}
	if v, ok := get("app_name"); ok {
		r.AppName = strings.TrimSpace(v)
	}
	if v, ok := get("category"); ok {
		r.Category = strings.TrimSpace(v)
	}
	if v, ok := get("app_description"); ok {
		r.AppDescription = strings.TrimSpace(v)
	}
	if v, ok := get("app_icp"); ok {
		r.AppICP = strings.TrimSpace(v)
	}
	if v, ok := get("url"); ok {
		r.URL = strings.TrimSpace(v)
	}
	if v, ok := get("service_name"); ok {
		r.ServiceName = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := get("status_code"); ok && strings.TrimSpace(v) != "" {
		n, err := parseIntField(sheet, line, "status_code", v)
		if err != nil {
			fail(err)
		} else {
			sc := int(n)
			r.StatusCode = &sc
		}
	}
	if v, ok := get("content_length"); ok && strings.TrimSpace(v) != "" {
		n, err := parseIntField(sheet, line, "content_length", v)
		if err != nil {
			fail(err)
		} else {
			r.ContentLength = &n
		}
	}
	if v, ok := get("page_title"); ok {
		r.PageTitle = strings.TrimSpace(v)
	}
	if v, ok := get("technologies"); ok {
		items, err := parseListField(sheet, line, "technologies", v)
		if err != nil {
			fail(err)
		} else {
			r.Technologies = items
		}
	}
	if v, ok := get("favicon_mmh3"); ok {
		r.FaviconMMH3 = strings.TrimSpace(v)
	}
	if v, ok := get("auth"); ok && strings.TrimSpace(v) != "" {
		auth, err := parseAuthJSON(sheet, line, v)
		if err != nil {
			fail(err)
		} else {
			r.Auth = auth
		}
	}
	if v, ok := get("port"); ok && strings.TrimSpace(v) != "" {
		n, err := parseIntField(sheet, line, "port", v)
		if err != nil {
			fail(err)
		} else {
			r.Port = int(n)
		}
	}
	if v, ok := get("method"); ok {
		r.Method = strings.TrimSpace(v)
	}
	if v, ok := get("params"); ok && strings.TrimSpace(v) != "" {
		ps, err := parseParamsJSON(sheet, line, v)
		if err != nil {
			fail(err)
		} else {
			r.Params = ps
		}
	}
	if v, ok := get("note"); ok {
		r.Note = strings.TrimSpace(v)
	}
	if cellErr != nil {
		return nil, cellErr
	}

	// CSV 的 service 行自动归类：有 URL → http；否则按其他服务。
	if csvMode && r.Type == "service" {
		if r.URL != "" {
			r.ServiceType = "http"
		} else {
			r.ServiceType = "other"
		}
	}
	return r, nil
}
