// Package assettransfer implements the asset import/export format contract from
// 《ARTEX RuoJi6 资产导入导出实施方案》(2026-11-16): parsing (XLSX/CSV/TXT/JSON),
// templates, export rendering, and row validation. Identity matching, merging and
// batch persistence live in db/asset_import.go; HTTP wiring in server/asset_transfer.go.
//
// 合同原则（方案 §6）：明确版本和字段白名单；固定中英文列名映射；未知 JSON
// 版本/结构拒绝；来源 ID 只作参考不直接覆盖目标实例。解析与预览全程不请求
// URL、不查 DNS、不执行公式。
package assettransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// 集中限制（方案 §13）。界面、模板和接口共用，不超限截断。
const (
	MaxUploadBytes   = 20 << 20  // 上传数据文件 20 MiB
	MaxRows          = 10000     // 单批数据行（跨工作表累计）
	MaxXLSXZipBytes  = 200 << 20 // XLSX 总解压量
	MaxXLSXEntries   = 1000      // XLSX ZIP 条目
	MaxCellBytes     = 64 << 10  // 单元格 64 KiB UTF-8
	MaxJSONDepth     = 32
	MaxXLSXCellChars = 32767 // Excel physical cell limit; independent of UTF-8 bytes

	PreviewTTL        = 30 * time.Minute   // 未提交预览有效期
	ReceiptTTL        = 7 * 24 * time.Hour // 仅含统计/错误状态的回执保留
	ExportRowsPerFile = MaxRows            // 导出分包行数上限（每包可完整回导）

	SchemaVersion   = 1
	FormatName      = "artex.assets"
	CSVSchemaMarker = "artex.assets.csv/1"
)

// AssetTypes is the fixed six-type whitelist.
var AssetTypes = []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"}

// Field carries one scalar/list field of a row. Only explicitly provided
// (non-empty) fields participate in fill/overwrite decisions (方案 §7.2/§7.3).
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"` // 显示/遮蔽后的值；仅用于预览差异展示
}

// SourceRef keeps origin metadata from an ARTEX JSON package. IDs are
// references only — they never bind to same-ID rows in the target instance
// (方案 §6.5).
type SourceRef struct {
	AssetID   int64   `json:"asset_id,omitempty"`
	CompanyID int64   `json:"company_id,omitempty"`
	TaskIDs   []int64 `json:"task_ids,omitempty"`
	LastSeen  string  `json:"last_seen,omitempty"`
}

// Row is one normalized incoming asset row. Empty = not provided by the file;
// import never treats a missing field as an instruction to clear (D05/A05).
type Row struct {
	Sheet string `json:"sheet,omitempty"` // 工作表名 / "粘贴" / "JSON"
	Line  int    `json:"line"`            // 数据行号（1 起，不含表头）
	Type  string `json:"type"`

	Domain       string      `json:"domain,omitempty"`
	RootDomain   string      `json:"root_domain,omitempty"` // 本地计算
	IP           string      `json:"ip,omitempty"`
	CSegment     string      `json:"c_segment,omitempty"` // 本地计算
	ICP          string      `json:"icp,omitempty"`
	BoundDomains []string    `json:"bound_domains,omitempty"`
	OpenPorts    []PortEntry `json:"open_ports,omitempty"`

	RecordType  string   `json:"record_type,omitempty"`
	RecordValue []string `json:"record_value,omitempty"`

	BundleID       string `json:"bundle_id,omitempty"`
	AppName        string `json:"app_name,omitempty"`
	Category       string `json:"category,omitempty"`
	AppDescription string `json:"app_description,omitempty"`
	AppICP         string `json:"app_icp,omitempty"`

	// URL 用于 service(http)/endpoint 的身份；app 行可选填，落 assets.url。
	URL           string      `json:"url,omitempty"`
	ServiceType   string      `json:"service_type,omitempty"` // http | other
	ServiceName   string      `json:"service_name,omitempty"`
	StatusCode    *int        `json:"status_code,omitempty"`
	ContentLength *int64      `json:"content_length,omitempty"`
	PageTitle     string      `json:"page_title,omitempty"`
	Technologies  []string    `json:"technologies,omitempty"`
	FaviconMMH3   string      `json:"favicon_mmh3,omitempty"`
	Auth          []AuthEntry `json:"auth,omitempty"`
	Port          int         `json:"port,omitempty"`

	Method string       `json:"method,omitempty"`
	Params []ParamEntry `json:"params,omitempty"`

	Note  string         `json:"note,omitempty"` // 映射 extra.note
	Extra map[string]any `json:"extra,omitempty"`

	SourceRef *SourceRef `json:"source_ref,omitempty"`
}

// PortEntry mirrors db.PortService without importing db.
type PortEntry struct {
	Port    int    `json:"port"`
	Service string `json:"service,omitempty"`
}

// AuthEntry mirrors db.AuthItem.
type AuthEntry struct {
	Type        string `json:"type,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	Token       string `json:"token,omitempty"`
	Description string `json:"description,omitempty"`
}

// ParamEntry mirrors db.ParamItem.
type ParamEntry struct {
	Location string `json:"location"`
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Type     string `json:"type,omitempty"`
}

// RowError is one rejected line with a Chinese reason (方案 §9.4).
type RowError struct {
	Sheet  string `json:"sheet,omitempty"`
	Line   int    `json:"line"`
	Type   string `json:"type,omitempty"`
	Ref    string `json:"ref,omitempty"` // 对象参考（域名/IP/URL，遮蔽敏感前缀后）
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason"`
}

// RowWarning is a non-fatal notice (e.g. unknown column not adopted).
type RowWarning struct {
	Sheet  string `json:"sheet,omitempty"`
	Detail string `json:"detail"`
}

// ParseResult is the outcome of parsing one upload/paste.
type ParseResult struct {
	Format   string        `json:"format"` // xlsx|csv|txt|json
	Rows     []*Row        `json:"rows"`
	Errors   []*RowError   `json:"errors,omitempty"`
	Warnings []*RowWarning `json:"warnings,omitempty"`
}

// Options is the per-batch import policy chosen in the UI (方案 §2 D03–D10).
type Options struct {
	Conflict      string `json:"conflict"`       // fill（默认补空）| overwrite（显式覆盖）
	CompanyID     int64  `json:"company_id"`     // 0 = 不指定
	TaskID        int64  `json:"task_id"`        // 0 = 不关联任务
	CompanyPolicy string `json:"company_policy"` // exclude（冲突行排除）| keep（保留原企业，导入其余）
}

func (o Options) Normalized() Options {
	if o.Conflict != "overwrite" {
		o.Conflict = "fill"
	}
	if o.CompanyPolicy != "keep" {
		o.CompanyPolicy = "exclude"
	}
	return o
}

// ParseError marks a whole-file rejection (unsupported format/version/contract).
type ParseError struct {
	Code    int // HTTP status suggestion (400/413/422)
	Message string
}

func (e *ParseError) Error() string { return e.Message }

func parseErr(code int, format string, args ...any) *ParseError {
	return &ParseError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Fingerprint returns a stable digest of the parsed input (rows+errors+format),
// used as the batch file fingerprint (方案 §9.1).
func (p *ParseResult) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "format=%s\nrows=%d\nerrors=%d\n", p.Format, len(p.Rows), len(p.Errors))
	for _, r := range p.Rows {
		fmt.Fprintf(h, "r|%s|%d|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%d\n",
			r.Sheet, r.Line, r.Type, r.Domain, r.IP, r.URL, r.AppName, r.BundleID,
			r.Port, r.RecordType, r.Method, r.ServiceName, len(r.OpenPorts))
	}
	for _, e := range p.Errors {
		fmt.Fprintf(h, "e|%s|%d|%s|%s\n", e.Sheet, e.Line, e.Type, e.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Label returns a short object reference of the row for preview/errors.
func (r *Row) Label() string {
	switch r.Type {
	case "root_domain", "subdomain":
		return r.Domain
	case "ip":
		return r.IP
	case "app":
		if r.BundleID != "" {
			return r.AppName + " (" + r.BundleID + ")"
		}
		return r.AppName
	case "service":
		if r.ServiceType == "other" {
			out := r.Domain
			if out == "" {
				out = r.IP
			}
			return fmt.Sprintf("%s:%d/%s", out, r.Port, r.ServiceName)
		}
		return r.URL
	case "endpoint":
		return r.Method + " " + r.URL
	}
	return ""
}
