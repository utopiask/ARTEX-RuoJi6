package assettransfer

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// colDef is one template/import column. zh/en are the accepted header names
// (fixed mapping — 不模糊猜测，重复/矛盾列名报错, 方案 §6.1).
type colDef struct {
	key  string
	zh   string
	en   string
	desc string // 填写说明
	req  bool   // 必填
	json bool   // JSON 列（数组/对象）
}

// 类型别名：CSV 的资产类型列接受英文键和中文类型名。
var typeAliases = map[string]string{
	"root_domain": "root_domain", "根域名": "root_domain",
	"ip": "ip", "IP": "ip",
	"subdomain": "subdomain", "子域名": "subdomain",
	"app": "app", "应用": "app",
	"service": "service", "服务": "service",
	"service_http": "service", "http服务": "service", "HTTP服务": "service",
	"service_other": "service", "其他服务": "service",
	"endpoint": "endpoint", "端点": "endpoint", "接口": "endpoint",
}

// sheetContract describes one XLSX worksheet. service 拆成 HTTP服务/其他服务两张表；
// CSV 统一一张表，service 行按 URL 有无自动归类。
type sheetContract struct {
	name  string // 工作表名
	typ   string // asset type（service 两张表都是 service）
	force string // service_type 强制值（http|other|""）
	cols  []colDef
}

var sheets = []sheetContract{
	{"根域名", "root_domain", "", []colDef{
		{"domain", "域名", "domain", "注册域名，如 example.com", true, false},
		{"icp", "ICP备案号", "icp", "可选，备案号", false, false},
		{"note", "备注", "note", "可选，保存到 extra.note", false, false},
	}},
	{"IP", "ip", "", []colDef{
		{"ip", "IP地址", "ip", "IPv4/IPv6", true, false},
		{"bound_domains", "绑定域名", "bound_domains", "分号分隔；也可填 JSON 数组", false, false},
		{"open_ports", "开放端口", "open_ports", `JSON 数组，如 [{"port":443,"service":"https"}]`, false, true},
		{"note", "备注", "note", "可选", false, false},
	}},
	{"子域名", "subdomain", "", []colDef{
		{"domain", "域名", "domain", "完整子域名", true, false},
		{"record_type", "记录类型", "record_type", "A/AAAA/CNAME，可留空", false, false},
		{"record_value", "记录值", "record_value", "分号分隔；或 JSON 数组", false, false},
		{"icp", "ICP备案号", "icp", "可选", false, false},
		{"note", "备注", "note", "可选", false, false},
	}},
	{"应用", "app", "", []colDef{
		{"app_name", "应用名称", "app_name", "必填", true, false},
		{"bundle_id", "BundleID", "bundle_id", "有则按它去重；无则按应用名", false, false},
		{"category", "分类", "category", "可选", false, false},
		{"app_description", "应用介绍", "app_description", "可选", false, false},
		{"app_icp", "应用ICP备案号", "app_icp", "可选", false, false},
		{"url", "链接", "url", "可选，应用相关链接", false, false},
		{"note", "备注", "note", "可选", false, false},
	}},
	{"HTTP服务", "service", "http", []colDef{
		{"url", "URL", "url", "http(s):// 开头，保留路径与查询", true, false},
		{"ip", "IP地址", "ip", "可选", false, false},
		{"status_code", "状态码", "status_code", "整数，可选", false, false},
		{"content_length", "内容长度", "content_length", "整数（字节），可选", false, false},
		{"page_title", "页面标题", "page_title", "可选", false, false},
		{"technologies", "技术", "technologies", "分号分隔；或 JSON 数组", false, false},
		{"favicon_mmh3", "favicon_mmh3", "favicon_mmh3", "可选", false, false},
		{"auth", "认证", "auth", `JSON 数组，键限 type/username/password/token/description/note/value/target，如 [{"type":"basic","username":"u"}]`, false, true},
		{"note", "备注", "note", "可选", false, false},
	}},
	{"其他服务", "service", "other", []colDef{
		{"domain", "域名", "domain", "域名或 IP 至少一项", false, false},
		{"ip", "IP地址", "ip", "域名或 IP 至少一项", false, false},
		{"port", "端口", "port", "1-65535，必填", true, false},
		{"service_name", "服务名", "service_name", "如 ssh/ftp，必填", true, false},
		{"auth", "认证", "auth", "JSON 数组，可选", false, true},
		{"note", "备注", "note", "可选", false, false},
	}},
	{"端点", "endpoint", "", []colDef{
		{"url", "URL", "url", "http(s):// 开头", true, false},
		{"method", "方法", "method", "GET/POST/…，必填", true, false},
		{"ip", "IP地址", "ip", "可选", false, false},
		{"params", "参数", "params", `JSON 数组，如 [{"location":"query","name":"id"}]`, false, true},
		{"note", "备注", "note", "可选", false, false},
	}},
}

// csvContract is the unified CSV sheet (方案 §6.3)：含资产类型列，元数据列由导出写入。
var csvTypeCol = colDef{"type", "资产类型", "type", "root_domain/ip/subdomain/app/service/endpoint", true, false}

func sheetByName(name string) *sheetContract {
	for i := range sheets {
		if strings.EqualFold(sheets[i].name, name) {
			return &sheets[i]
		}
	}
	return nil
}

// headerIndex maps a header cell (zh or en, trimmed) to a column key.
// 重复列名 / 同列中英文都出现 → 报错（矛盾列名）。
func buildHeaderMap(sheet string, line int, headers []string, contract []colDef) (map[int]string, []string, *RowError) {
	seen := map[string]int{} // key -> header 列下标
	out := map[int]string{}
	var warnings []string
	for idx, h := range headers {
		h = strings.TrimSpace(h)
		h = strings.TrimSuffix(h, "*") // 模板必填标记
		if h == "" {
			continue
		}
		matched := ""
		for _, c := range contract {
			if strings.EqualFold(h, c.zh) || strings.EqualFold(h, c.en) {
				matched = c.key
				break
			}
		}
		if matched == "" {
			warnings = append(warnings, fmt.Sprintf("%s 表第 %d 列「%s」未采用：不是合同列名", sheet, idx+1, h))
			continue
		}
		if prev, dup := seen[matched]; dup {
			return nil, nil, &RowError{Sheet: sheet, Line: line, Field: matched,
				Reason: fmt.Sprintf("列「%s」重复出现（第 %d 列和第 %d 列）", h, prev+1, idx+1)}
		}
		seen[matched] = idx
		out[idx] = matched
	}
	return out, warnings, nil
}

// ---- 字段级解析与校验 ----

func parseDomain(v string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
}

func parseIPField(sheet string, line int, field, v string) (string, *RowError) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	ip := net.ParseIP(v)
	if ip == nil {
		return "", &RowError{Sheet: sheet, Line: line, Field: field,
			Reason: fmt.Sprintf("ip 必须是 IPv4/IPv6 地址，收到 %q；主机名请登记为子域名", truncateForReason(v))}
	}
	return v, nil
}

func parseIntField(sheet string, line int, field, v string) (int64, *RowError) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, &RowError{Sheet: sheet, Line: line, Field: field,
			Reason: fmt.Sprintf("%s 必须是整数，收到 %q", field, truncateForReason(v))}
	}
	return n, nil
}

// parseListField 接受分号分隔或 JSON 数组（方案 §6.2）。
func parseListField(sheet string, line int, field, v string) ([]string, *RowError) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if strings.HasPrefix(v, "[") {
		var items []string
		if err := decodeStrictJSON(v, MaxJSONDepth, &items); err != nil {
			return nil, &RowError{Sheet: sheet, Line: line, Field: field, Reason: field + " JSON 数组无效: " + err.Error()}
		}
		return dedupeNonEmpty(items), nil
	}
	parts := strings.Split(v, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return dedupeNonEmpty(out), nil
}

func dedupeNonEmpty(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func truncateForReason(v string) string {
	const max = 40
	if len(v) > max {
		return v[:max] + "…"
	}
	return v
}

// validateRowType 填充派生字段并做跨字段校验；返回行级错误。
func finalizeRow(r *Row) *RowError {
	switch r.Type {
	case "root_domain":
		if r.Domain == "" {
			return rowErr(r, "domain", "根域名行缺少域名")
		}
		r.RootDomain = r.Domain
	case "ip":
		if r.IP == "" {
			return rowErr(r, "ip", "IP 行缺少 IP 地址")
		}
		r.CSegment = calcCSegmentLocal(r.IP)
	case "subdomain":
		if r.Domain == "" {
			return rowErr(r, "domain", "子域名行缺少域名")
		}
		rt := strings.ToUpper(strings.TrimSpace(r.RecordType))
		if rt != "" && rt != "A" && rt != "AAAA" && rt != "CNAME" && rt != "TXT" && rt != "MX" && rt != "NS" && rt != "PTR" && rt != "SRV" {
			return rowErr(r, "record_type", "记录类型 "+rt+" 不被支持（A/AAAA/CNAME/TXT/MX/NS/PTR/SRV 或留空）")
		}
		r.RecordType = rt
		root, _ := RootDomainLocal(r.Domain)
		if root == "" {
			root = r.Domain
		}
		r.RootDomain = root
	case "app":
		if r.AppName == "" {
			return rowErr(r, "app_name", "应用行缺少应用名称")
		}
	case "service":
		if r.ServiceType == "http" {
			if r.URL == "" {
				return rowErr(r, "url", "HTTP 服务行缺少 URL")
			}
			if !strings.HasPrefix(strings.ToLower(r.URL), "http://") && !strings.HasPrefix(strings.ToLower(r.URL), "https://") {
				return rowErr(r, "url", "HTTP 服务 URL 必须以 http:// 或 https:// 开头")
			}
			if r.IP != "" {
				r.CSegment = calcCSegmentLocal(r.IP)
			}
		} else {
			if r.Domain == "" && r.IP == "" {
				return rowErr(r, "domain", "其他服务行域名与 IP 至少填一项")
			}
			if r.Port < 1 || r.Port > 65535 {
				return rowErr(r, "port", "其他服务端口必须在 1-65535")
			}
			if r.ServiceName == "" {
				return rowErr(r, "service_name", "其他服务行缺少服务名")
			}
			if r.IP != "" {
				r.CSegment = calcCSegmentLocal(r.IP)
			}
		}
	case "endpoint":
		if r.URL == "" {
			return rowErr(r, "url", "端点行缺少 URL")
		}
		if !strings.HasPrefix(strings.ToLower(r.URL), "http://") && !strings.HasPrefix(strings.ToLower(r.URL), "https://") {
			return rowErr(r, "url", "端点 URL 必须以 http:// 或 https:// 开头")
		}
		m := strings.ToUpper(strings.TrimSpace(r.Method))
		if !validMethod(m) {
			return rowErr(r, "method", "HTTP 方法无效: "+truncateForReason(r.Method))
		}
		r.Method = m
		if r.IP != "" {
			r.CSegment = calcCSegmentLocal(r.IP)
		}
	default:
		return rowErr(r, "type", "未知资产类型: "+r.Type)
	}
	if r.Port != 0 && (r.Port < 1 || r.Port > 65535) {
		return rowErr(r, "port", "端口必须在 1-65535")
	}
	return nil
}

var knownMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true,
	"HEAD": true, "OPTIONS": true, "TRACE": true, "CONNECT": true,
}

func validMethod(m string) bool { return knownMethods[m] }

func rowErr(r *Row, field, reason string) *RowError {
	return &RowError{Sheet: r.Sheet, Line: r.Line, Type: r.Type, Ref: truncateForReason(r.Label()), Field: field, Reason: reason}
}
