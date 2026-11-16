package assettransfer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
)

// ExportAsset mirrors the export-relevant fields of db.Asset. db 不能被本包
// import（db/asset_import.go 依赖本包），由 server 层完成映射。
type ExportAsset struct {
	ID        int64   `json:"-"`
	Type      string  `json:"-"`
	CompanyID *int64  `json:"-"`
	TaskIDs   []int64 `json:"-"`
	LastSeen  string  `json:"-"`

	Domain         string         `json:"domain,omitempty"`
	RootDomain     string         `json:"root_domain,omitempty"`
	IP             string         `json:"ip,omitempty"`
	CSegment       string         `json:"c_segment,omitempty"`
	ICP            string         `json:"icp,omitempty"`
	BoundDomains   []string       `json:"bound_domains,omitempty"`
	OpenPorts      []PortEntry    `json:"open_ports,omitempty"`
	RecordType     string         `json:"record_type,omitempty"`
	RecordValue    []string       `json:"record_value,omitempty"`
	BundleID       string         `json:"bundle_id,omitempty"`
	AppName        string         `json:"app_name,omitempty"`
	Category       string         `json:"category,omitempty"`
	AppDescription string         `json:"app_description,omitempty"`
	AppICP         string         `json:"app_icp,omitempty"`
	URL            string         `json:"url,omitempty"`
	ServiceType    string         `json:"service_type,omitempty"`
	ServiceName    string         `json:"service_name,omitempty"`
	Port           int            `json:"port,omitempty"`
	FaviconMMH3    string         `json:"favicon_mmh3,omitempty"`
	StatusCode     *int           `json:"status_code,omitempty"`
	ContentLength  *int64         `json:"content_length,omitempty"`
	PageTitle      string         `json:"page_title,omitempty"`
	Technologies   []string       `json:"technologies,omitempty"`
	Auth           []AuthEntry    `json:"auth,omitempty"`
	Method         string         `json:"method,omitempty"`
	Params         []ParamEntry   `json:"params,omitempty"`
	Extra          map[string]any `json:"-"`
}

// ExportOptions controls one export render.
type ExportOptions struct {
	Format           string // xlsx|csv|json
	IncludeSensitive bool   // 默认排除 auth/extra/params 值（D08/§10.3）
	GeneratedAt      time.Time
	ScopeDesc        string // 范围描述（写入包清单/JSON source）
	PartIndex        int    // 1 起
	PartCount        int
}

// OmittedFields lists the fields dropped under the current policy. 必须与实际
// 省略一致（§6.5/§10.3），回填时缺失字段不清除。
func OmittedFields(includeSensitive bool) []string {
	if includeSensitive {
		return []string{}
	}
	return []string{"auth", "extra_except_note", "params_values"}
}

// RenderPart renders one file of the export. name 不含扩展名前缀由调用方拼。
func RenderPart(opts ExportOptions, assets []ExportAsset) (ext string, data []byte, err error) {
	// Apply policy once before any format serializer. Copy nested mutable values.
	safe := make([]ExportAsset, len(assets))
	for i, a := range assets {
		if !opts.IncludeSensitive {
			if sensitiveURL(a.URL) {
				return "", nil, fmt.Errorf("asset %d: URL contains sensitive credentials; explicitly enable include_sensitive to export it", i+1)
			}
			a.Auth = nil
			a.Params = append([]ParamEntry(nil), a.Params...)
			for j := range a.Params {
				a.Params[j].Value = ""
			}
			a.Extra = map[string]any{"note": a.Extra["note"]}
		}
		if err := validateExportCells(a, opts.Format); err != nil {
			return "", nil, fmt.Errorf("asset %d: %w", i+1, err)
		}
		safe[i] = a
	}
	assets = safe
	switch opts.Format {
	case "xlsx":
		b, err := renderXLSX(assets)
		return "xlsx", b, err
	case "csv":
		return "csv", renderCSV(assets, opts), nil
	case "json":
		return "json", renderJSON(opts, assets), nil
	}
	return "", nil, fmt.Errorf("unknown export format %q", opts.Format)
}

// sensitiveURL inspects locally and never includes URL content in errors.
func sensitiveURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return raw != ""
	} // malformed URLs cannot be safely classified
	if u.User != nil {
		return true
	}
	for key := range u.Query() {
		k := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
		for _, secret := range []string{"token", "password", "passwd", "secret", "apikey", "authorization", "credential", "signature", "session", "accesskey"} {
			if strings.Contains(k, secret) {
				return true
			}
		}
		if k == "auth" || k == "key" || k == "pwd" || k == "jwt" {
			return true
		}
	}
	return false
}

func validateExportCells(a ExportAsset, format string) error {
	// JSON has arbitrary extra values; validate their serialized field sizes too.
	b, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("asset structure cannot be encoded")
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	extra := make(map[string]any, len(a.Extra))
	for k, v := range a.Extra {
		if k != "note" {
			extra[k] = v
		}
	}
	m["extra"] = extra
	if err := checkJSONCellSizes(m); err != nil {
		return err
	}
	for _, c := range csvUnionColumns() {
		text := fmt.Sprint(exportCell(a, c.key))
		if len(text) > MaxCellBytes {
			return fmt.Errorf("field %s exceeds %d byte cell limit", c.key, MaxCellBytes)
		}
		if format == "xlsx" && utf8.RuneCountInString(text) > MaxXLSXCellChars {
			return fmt.Errorf("field %s exceeds XLSX %d character cell limit; use CSV or JSON", c.key, MaxXLSXCellChars)
		}
	}
	return nil
}

// ---- XLSX ----

func renderXLSX(assets []ExportAsset) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	if err := f.SetSheetName("Sheet1", sheets[0].name); err != nil {
		return nil, err
	}
	for i := 1; i < len(sheets); i++ { // 第 0 张由 SetSheetName 提供
		if _, err := f.NewSheet(sheets[i].name); err != nil {
			return nil, err
		}
	}
	rowIdx := map[string]int{}
	for _, sh := range sheets {
		headers := make([]string, 0, len(sh.cols)+1)
		headers = append(headers, "ID")
		for _, c := range sh.cols {
			headers = append(headers, c.zh)
		}
		cell, _ := excelize.CoordinatesToCellName(1, 1)
		if err := f.SetSheetRow(sh.name, cell, &headers); err != nil {
			return nil, err
		}
		rowIdx[sh.name] = 1
	}
	for _, a := range assets {
		sh := exportSheetFor(a)
		if sh == nil {
			continue
		}
		rowIdx[sh.name]++
		values := exportRowValues(a, *sh)
		if err := setSheetRowSafe(f, sh.name, rowIdx[sh.name], values); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func exportSheetFor(a ExportAsset) *sheetContract {
	switch a.Type {
	case "root_domain":
		return sheetByName("根域名")
	case "ip":
		return sheetByName("IP")
	case "subdomain":
		return sheetByName("子域名")
	case "app":
		return sheetByName("应用")
	case "endpoint":
		return sheetByName("端点")
	case "service":
		if a.ServiceType == "http" {
			return sheetByName("HTTP服务")
		}
		return sheetByName("其他服务")
	}
	return nil
}

// exportRowValues produces one row: ID + each contract column. 数值以数值类型
// 写入，其余全部 SetCellStr 文本——用户内容永不进公式接口（§6.2）。
func exportRowValues(a ExportAsset, sh sheetContract) []any {
	out := make([]any, 0, len(sh.cols)+1)
	out = append(out, a.ID)
	for _, c := range sh.cols {
		out = append(out, exportCell(a, c.key))
	}
	return out
}

func setSheetRowSafe(f *excelize.File, sheet string, row int, values []any) error {
	for i, v := range values {
		cell, err := excelize.CoordinatesToCellName(i+1, row)
		if err != nil {
			return err
		}
		switch x := v.(type) {
		case string:
			if len(x) > MaxCellBytes || utf8.RuneCountInString(x) > MaxXLSXCellChars {
				return fmt.Errorf("XLSX cell %s exceeds byte or character limit", cell)
			}
			if err := f.SetCellStr(sheet, cell, x); err != nil {
				return err
			}
		case int:
			if err := f.SetCellDefault(sheet, cell, strconv.Itoa(x)); err != nil {
				return err
			}
		case int64:
			if err := f.SetCellDefault(sheet, cell, strconv.FormatInt(x, 10)); err != nil {
				return err
			}
		default:
			if err := f.SetCellStr(sheet, cell, fmt.Sprint(x)); err != nil {
				return err
			}
		}
	}
	return nil
}

func exportCell(a ExportAsset, key string) any {
	switch key {
	case "domain":
		return a.Domain
	case "ip":
		return a.IP
	case "icp":
		return a.ICP
	case "record_type":
		return a.RecordType
	case "record_value":
		return exportList(a.RecordValue)
	case "bound_domains":
		return exportList(a.BoundDomains)
	case "open_ports":
		if len(a.OpenPorts) == 0 {
			return ""
		}
		return marshalCompact(portEntriesToAny(a.OpenPorts))
	case "bundle_id":
		return a.BundleID
	case "app_name":
		return a.AppName
	case "category":
		return a.Category
	case "app_description":
		return a.AppDescription
	case "app_icp":
		return a.AppICP
	case "url":
		return a.URL
	case "service_name":
		return a.ServiceName
	case "status_code":
		if a.StatusCode == nil {
			return ""
		}
		return *a.StatusCode
	case "content_length":
		if a.ContentLength == nil {
			return ""
		}
		return *a.ContentLength
	case "page_title":
		return a.PageTitle
	case "technologies":
		return exportList(a.Technologies)
	case "favicon_mmh3":
		return a.FaviconMMH3
	case "auth":
		if len(a.Auth) == 0 {
			return ""
		}
		return marshalCompact(authEntriesToAny(a.Auth))
	case "port":
		if a.Port == 0 {
			return ""
		}
		return a.Port
	case "method":
		return a.Method
	case "params":
		if len(a.Params) == 0 {
			return ""
		}
		return marshalCompact(paramEntriesToAny(a.Params))
	case "note":
		if a.Extra == nil {
			return ""
		}
		if v, ok := a.Extra["note"].(string); ok {
			return v
		}
		return ""
	}
	return ""
}

func portEntriesToAny(in []PortEntry) []any {
	out := make([]any, len(in))
	for i, p := range in {
		m := map[string]any{"port": p.Port}
		if p.Service != "" {
			m["service"] = p.Service
		}
		out[i] = m
	}
	return out
}

func authEntriesToAny(in []AuthEntry) []any {
	out := make([]any, len(in))
	for i, e := range in {
		b, _ := json.Marshal(e)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out[i] = m
	}
	return out
}

func paramEntriesToAny(in []ParamEntry) []any {
	out := make([]any, len(in))
	for i, p := range in {
		b, _ := json.Marshal(p)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out[i] = m
	}
	return out
}

// ---- CSV ----

// csvEscape prefixes one layer of ' before formula-significant leading chars
// (=+-@ 以及本身以 ' 开头的值)，配合 _artex_csv_text 标记可无损回剥（§6.3/A18）。
func csvEscape(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '\'', '\t', '\r':
		return "'" + v
	}
	return v
}

func renderCSV(assets []ExportAsset, opts ExportOptions) []byte {
	all := append([]colDef{csvTypeCol}, csvUnionColumns()...)
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	headers := make([]string, 0, len(all)+2)
	for _, c := range all {
		headers = append(headers, c.en)
	}
	headers = append(headers, "_artex_schema", "_artex_csv_text")
	_ = w.Write(headers)
	// 元数据行：声明本格式与被转义列集合（全部文本列都参与转义规则）
	escapedCols := make([]string, 0, len(all))
	for _, c := range all {
		escapedCols = append(escapedCols, c.en)
	}
	meta := make([]string, len(all))
	_ = w.Write(append(meta, CSVSchemaMarker, strings.Join(escapedCols, ";")))
	for _, a := range assets {
		rec := make([]string, 0, len(all))
		rec = append(rec, a.Type)
		for _, c := range all {
			if c.key == "type" {
				continue // type 已作为首列写入
			}
			var raw string
			switch v := exportCell(a, c.key).(type) {
			case string:
				raw = v
			case int:
				raw = strconv.Itoa(v)
			case int64:
				raw = strconv.FormatInt(v, 10)
			default:
				raw = fmt.Sprint(v)
			}
			rec = append(rec, csvEscape(raw))
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes()
}

// ---- JSON ----

type jsonExportPackage struct {
	Format        string            `json:"format"`
	SchemaVersion int               `json:"schema_version"`
	ExportedAt    string            `json:"exported_at"`
	Source        jsonSource        `json:"source"`
	ExportPolicy  jsonPolicy        `json:"export_policy"`
	Assets        []json.RawMessage `json:"assets"`
}

type jsonSource struct {
	Application string `json:"application"`
	PartIndex   int    `json:"part_index"`
	PartCount   int    `json:"part_count"`
	Scope       string `json:"scope,omitempty"`
}

type jsonPolicy struct {
	IncludeSensitive bool     `json:"include_sensitive"`
	OmittedFields    []string `json:"omitted_fields"`
}

func renderJSON(opts ExportOptions, assets []ExportAsset) []byte {
	pkg := jsonExportPackage{
		Format:        FormatName,
		SchemaVersion: SchemaVersion,
		ExportedAt:    opts.GeneratedAt.UTC().Format(time.RFC3339),
		Source: jsonSource{
			Application: "ARTEX-RuoJi6",
			PartIndex:   opts.PartIndex,
			PartCount:   opts.PartCount,
			Scope:       opts.ScopeDesc,
		},
		ExportPolicy: jsonPolicy{IncludeSensitive: opts.IncludeSensitive, OmittedFields: OmittedFields(opts.IncludeSensitive)},
		Assets:       []json.RawMessage{},
	}
	for _, a := range assets {
		m := map[string]any{"type": a.Type}
		put := func(k string, v any) {
			switch x := v.(type) {
			case string:
				if x != "" {
					m[k] = x
				}
			case []string:
				if len(x) > 0 {
					m[k] = x
				}
			default:
				if v != nil {
					m[k] = v
				}
			}
		}
		switch a.Type {
		case "root_domain":
			put("domain", a.Domain)
			put("icp", a.ICP)
		case "ip":
			put("ip", a.IP)
			put("bound_domains", a.BoundDomains)
			if len(a.OpenPorts) > 0 {
				put("open_ports", portEntriesToAny(a.OpenPorts))
			}
		case "subdomain":
			put("domain", a.Domain)
			put("record_type", a.RecordType)
			put("record_value", a.RecordValue)
			put("icp", a.ICP)
		case "app":
			put("app_name", a.AppName)
			put("bundle_id", a.BundleID)
			put("category", a.Category)
			put("app_description", a.AppDescription)
			put("app_icp", a.AppICP)
			put("url", a.URL)
		case "service":
			if a.ServiceType == "http" {
				put("url", a.URL)
				put("ip", a.IP)
				if a.StatusCode != nil {
					put("status_code", *a.StatusCode)
				}
				if a.ContentLength != nil {
					put("content_length", *a.ContentLength)
				}
				put("page_title", a.PageTitle)
				put("technologies", a.Technologies)
				put("favicon_mmh3", a.FaviconMMH3)
			} else {
				put("domain", a.Domain)
				put("ip", a.IP)
				put("port", a.Port)
				put("service_name", a.ServiceName)
			}
			if opts.IncludeSensitive && len(a.Auth) > 0 {
				put("auth", authEntriesToAny(a.Auth))
			}
		case "endpoint":
			put("url", a.URL)
			put("method", a.Method)
			put("ip", a.IP)
			if len(a.Params) > 0 {
				put("params", paramsForExport(a.Params, opts.IncludeSensitive))
			}
		}
		if v, ok := a.Extra["note"].(string); ok && v != "" {
			put("note", v)
		}
		if opts.IncludeSensitive && len(a.Extra) > 0 {
			clean := map[string]any{}
			for k, v := range a.Extra {
				clean[k] = v
			}
			delete(clean, "note")
			if len(clean) > 0 {
				put("extra", clean)
			}
		}
		src := map[string]any{"asset_id": a.ID}
		if a.CompanyID != nil {
			src["company_id"] = *a.CompanyID
		}
		if len(a.TaskIDs) > 0 {
			src["task_ids"] = a.TaskIDs
		}
		if a.LastSeen != "" {
			src["last_seen"] = a.LastSeen
		}
		m["source_ref"] = src
		b, _ := json.Marshal(m)
		pkg.Assets = append(pkg.Assets, b)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(pkg)
	return buf.Bytes()
}

func paramsForExport(in []ParamEntry, withValues bool) []any {
	out := make([]any, len(in))
	for i, p := range in {
		m := map[string]any{"location": p.Location, "name": p.Name}
		if withValues && p.Value != "" {
			m["value"] = p.Value
		}
		if p.Type != "" {
			m["type"] = p.Type
		}
		out[i] = m
	}
	return out
}

func exportList(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return marshalCompact(v)
}
