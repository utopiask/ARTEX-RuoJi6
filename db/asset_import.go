package db

// 资产导入批次（实施方案 §7–§9）：身份匹配沿用 assets 表唯一索引语义；默认
// 补空、显式覆盖、企业/任务归属按 D03–D10；提交在同一事务完成并写回执。
//
// 本文件只做受控导入合并路径，不复用普通 Upsert（Upsert 会覆盖非空标量、
// 生成派生资产并刷新 last_seen，见方案 §4.2）。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/assettransfer"
)

// =====================================================================
// 数据模型
// =====================================================================

// ImportSummary counts rows by planned action (方案 §9.1)。
type ImportSummary struct {
	Create    int `json:"create"`
	Fill      int `json:"fill"`
	Overwrite int `json:"overwrite"`
	Unchanged int `json:"unchanged"`
	Conflict  int `json:"conflict"`
	Invalid   int `json:"invalid"`
	Total     int `json:"total"`
}

// ImportDiff is one field-level difference shown in the preview. 敏感值只给
// 摘要（kind=mask），不回传明文（§9.2：不把遮蔽后的认证字符串写回数据库）。
type ImportDiff struct {
	Field string `json:"field"`
	Kind  string `json:"kind"` // fill|change|add|remove|keep|mask
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
	Note  string `json:"note,omitempty"`
}

// ImportPlanRow is one row's plan in a batch.
type ImportPlanRow struct {
	Index       int                `json:"index"` // 批内行下标（提交行选择引用）
	Row         *assettransfer.Row `json:"row"`
	Action      string             `json:"action"` // create|fill|overwrite|unchanged|conflict|invalid
	AssetID     int64              `json:"asset_id,omitempty"`
	Diffs       []ImportDiff       `json:"diffs,omitempty"`
	SharedTasks []int64            `json:"shared_with_tasks,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	// Recomputed under commit locks; never exposed or persisted with the preview.
	updates map[string]any
}

// ImportPlan is the full preview payload persisted with the batch.
type ImportPlan struct {
	Rows    []*ImportPlanRow `json:"rows"`
	Summary ImportSummary    `json:"summary"`
	Hash    string           `json:"hash"`
}

// ImportReceipt is the commit result (不含明文认证；仅统计与 ID)。
type ImportReceipt struct {
	BatchID     int64         `json:"batch_id"`
	CommittedAt string        `json:"committed_at"`
	Summary     ImportSummary `json:"summary"`
	CreatedIDs  []int64       `json:"created_ids,omitempty"`
	UpdatedIDs  []int64       `json:"updated_ids,omitempty"`
	Attached    int           `json:"attached"`
	Existing    int           `json:"existing_links"`
}

// AssetImportBatch is one persisted batch.
type AssetImportBatch struct {
	ID          int64
	Status      string
	FileName    string
	FileFormat  string
	FileSHA     string
	Options     assettransfer.Options
	Rows        []*assettransfer.Row
	RowErrors   []*assettransfer.RowError
	Warnings    []*assettransfer.RowWarning
	Plan        *ImportPlan
	Receipt     *ImportReceipt
	CreatedAt   time.Time
	ExpiresAt   time.Time
	CommittedAt *time.Time
}

var (
	ErrImportBatchNotFound   = errors.New("import batch not found")
	ErrImportBatchExpired    = errors.New("import batch expired")
	ErrImportBatchStale      = errors.New("preview is stale, re-preview required")
	ErrImportCompanyNotFound = errors.New("company not found")
	ErrImportTaskNotFound    = errors.New("task not found")
	ErrImportTaskDeleted     = errors.New("task deleted")
)

// =====================================================================
// 身份匹配（沿用业务唯一键，§7.1）
// =====================================================================

type importIdentity struct {
	typ string
	a   string
	b   string
}

func rowIdentity(r *assettransfer.Row) importIdentity {
	switch r.Type {
	case "root_domain":
		return importIdentity{"root_domain", DomainKey(r.Domain), ""}
	case "ip":
		return importIdentity{"ip", canonicalIP(r.IP), ""}
	case "subdomain":
		return importIdentity{"subdomain", DomainKey(r.Domain), r.RecordType}
	case "app":
		if r.BundleID != "" {
			return importIdentity{"app", "bundle:" + r.BundleID, ""}
		}
		return importIdentity{"app", "name:" + r.AppName, ""}
	case "service":
		if r.ServiceType == "http" {
			return importIdentity{"service", normalizeURL(r.URL), "http"}
		}
		d := DomainKey(r.Domain)
		return importIdentity{"service", fmt.Sprintf("%s|%s|%d", d, canonicalIP(r.IP), r.Port), strings.ToLower(r.ServiceName)}
	case "endpoint":
		return importIdentity{"endpoint", normalizeURL(r.URL), strings.ToUpper(r.Method)}
	}
	return importIdentity{}
}

func canonicalIP(ip string) string {
	if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil {
		return parsed.String()
	}
	return strings.TrimSpace(ip)
}

type importQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// matchAssetsForImport loads existing assets for the given rows using the same
// keys as the unique indexes. Returns matched assets per identity plus
// ambiguous identities (multiple rows match → 歧义冲突，不任挑一个, §7.1)。
func matchAssetsForImport(q importQuerier, rows []*assettransfer.Row) (map[importIdentity]*Asset, map[importIdentity][]*Asset, error) {
	collect := map[string][]string{
		"root_domain": {}, "ip": {}, "subdomain": {}, "bundle": {}, "appname": {}, "url": {}, "domain": {}, "portip": {},
	}
	for _, r := range rows {
		switch r.Type {
		case "root_domain":
			collect["root_domain"] = append(collect["root_domain"], DomainKey(r.Domain))
		case "ip":
			collect["ip"] = append(collect["ip"], strings.TrimSpace(r.IP), canonicalIP(r.IP))
		case "subdomain":
			collect["subdomain"] = append(collect["subdomain"], DomainKey(r.Domain))
		case "app":
			if r.BundleID != "" {
				collect["bundle"] = append(collect["bundle"], r.BundleID)
			} else {
				collect["appname"] = append(collect["appname"], r.AppName)
			}
		case "service":
			if r.ServiceType == "http" {
				collect["url"] = append(collect["url"], normalizeURL(r.URL))
			} else {
				if d := DomainKey(r.Domain); d != "" {
					collect["domain"] = append(collect["domain"], d)
				}
				if ip := strings.TrimSpace(r.IP); ip != "" {
					collect["portip"] = append(collect["portip"], ip, canonicalIP(ip))
				}
			}
		case "endpoint":
			collect["url"] = append(collect["url"], normalizeURL(r.URL))
		}
	}

	existing := map[importIdentity]*Asset{}
	ambiguous := map[importIdentity][]*Asset{}

	load := func(typ, serviceType, where string, args ...any) error {
		qs := assetSelectCols + " WHERE type=$1" + where
		all := append([]any{typ}, args...)
		rowsQ, err := q.Query(qs, all...)
		if err != nil {
			return err
		}
		defer rowsQ.Close()
		assets, err := scanAssets(rowsQ)
		if err != nil {
			return err
		}
		for _, a := range assets {
			id := assetIdentityOf(a)
			if prev, dup := existing[id]; dup && prev.ID != a.ID {
				ambiguous[id] = append(ambiguous[id], a)
			} else {
				existing[id] = a
			}
		}
		return nil
	}

	if len(collect["root_domain"]) > 0 {
		if err := load("root_domain", "", ` AND domain = ANY($2::text[])`, toArray(collect["root_domain"])); err != nil {
			return nil, nil, err
		}
	}
	if len(collect["ip"]) > 0 {
		// 文本等值 + try_inet 归一两种方式都匹配，捕捉库内等价写法的旧行；
		// 多行同身份由上面的 ambiguous 逻辑显式报冲突（§7.1）。
		if err := load("ip", "", ` AND (ip = ANY($2::text[]) OR host(try_inet(ip)) = ANY($2::text[]))`, toArray(collect["ip"])); err != nil {
			return nil, nil, err
		}
	}
	if len(collect["subdomain"]) > 0 {
		if err := load("subdomain", "", ` AND domain = ANY($2::text[])`, toArray(collect["subdomain"])); err != nil {
			return nil, nil, err
		}
	}
	if len(collect["bundle"]) > 0 {
		if err := load("app", "", ` AND bundle_id = ANY($2::text[])`, toArray(collect["bundle"])); err != nil {
			return nil, nil, err
		}
	}
	if len(collect["appname"]) > 0 {
		if err := load("app", "", ` AND bundle_id IS NULL AND app_name = ANY($2::text[])`, toArray(collect["appname"])); err != nil {
			return nil, nil, err
		}
	}
	if len(collect["url"]) > 0 {
		if err := load("service", "http", ` AND service_type='http' AND url = ANY($2::text[])`, toArray(collect["url"])); err != nil {
			return nil, nil, err
		}
		if err := load("endpoint", "", ` AND url = ANY($2::text[])`, toArray(collect["url"])); err != nil {
			return nil, nil, err
		}
	}
	// 其他服务：按 domain/ip 拉候选后在 Go 里按四元组匹配
	if len(collect["domain"]) > 0 || len(collect["portip"]) > 0 {
		var candidates []*Asset
		if len(collect["domain"]) > 0 {
			rowsQ, err := q.Query(assetSelectCols+` WHERE type='service' AND service_type='other' AND domain = ANY($1::text[])`, toArray(collect["domain"]))
			if err != nil {
				return nil, nil, err
			}
			candidates, err = scanAndClose(rowsQ)
			if err != nil {
				return nil, nil, err
			}
		}
		if len(collect["portip"]) > 0 {
			rowsQ, err := q.Query(assetSelectCols+` WHERE type='service' AND service_type='other' AND ip = ANY($1::text[])`, toArray(collect["portip"]))
			if err != nil {
				return nil, nil, err
			}
			more, err := scanAndClose(rowsQ)
			if err != nil {
				return nil, nil, err
			}
			candidates = append(candidates, more...)
		}
		for _, a := range candidates {
			id := assetIdentityOf(a)
			if prev, dup := existing[id]; dup && prev.ID != a.ID {
				ambiguous[id] = append(ambiguous[id], a)
			} else {
				existing[id] = a
			}
		}
	}
	return existing, ambiguous, nil
}

func scanAndClose(rows *sql.Rows) ([]*Asset, error) {
	defer rows.Close()
	return scanAssets(rows)
}

func assetIdentityOf(a *Asset) importIdentity {
	switch a.Type {
	case "root_domain":
		return importIdentity{"root_domain", DomainKey(a.Domain), ""}
	case "ip":
		return importIdentity{"ip", canonicalIP(a.IP), ""}
	case "subdomain":
		return importIdentity{"subdomain", DomainKey(a.Domain), a.RecordType}
	case "app":
		if a.BundleID != "" {
			return importIdentity{"app", "bundle:" + a.BundleID, ""}
		}
		return importIdentity{"app", "name:" + a.AppName, ""}
	case "service":
		if a.ServiceType == "http" {
			return importIdentity{"service", normalizeURL(a.URL), "http"}
		}
		port := 0
		if a.Port != nil {
			port = *a.Port
		}
		return importIdentity{"service", fmt.Sprintf("%s|%s|%d", DomainKey(a.Domain), canonicalIP(a.IP), port), strings.ToLower(a.ServiceName)}
	case "endpoint":
		return importIdentity{"endpoint", normalizeURL(a.URL), strings.ToUpper(a.Method)}
	}
	return importIdentity{}
}

func toArray(in []string) []string {
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

// =====================================================================
// 计划计算（预览差异）
// =====================================================================

// ComputeImportPlan builds the full plan for parsed rows. 纯读路径：不写资产、
// 不写关联、不改范围、零 DNS/HTTP（A10）。
func ComputeImportPlan(q importQuerier, rows []*assettransfer.Row, rowErrors []*assettransfer.RowError, opts assettransfer.Options) (*ImportPlan, error) {
	opts = opts.Normalized()

	// 文件内同身份合并（§7.1）：兼容信息合并，矛盾标量显式冲突
	merged, conflicts := mergeInFile(rows)
	existing, ambiguous, err := matchAssetsForImport(q, merged)
	if err != nil {
		return nil, err
	}

	plan := &ImportPlan{Rows: make([]*ImportPlanRow, 0, len(rows))}
	// 无效行
	errByLine := map[int]*assettransfer.RowError{}
	for _, e := range rowErrors {
		errByLine[e.Line] = e
		plan.Rows = append(plan.Rows, &ImportPlanRow{
			Index: len(plan.Rows), Row: &assettransfer.Row{Sheet: e.Sheet, Line: e.Line, Type: e.Type},
			Action: "invalid", Reason: e.Reason,
		})
		plan.Summary.Invalid++
	}

	// 共享任务引用：一次查全部将被更新的资产
	sharedLookup := map[int64][]int64{}
	updateCandidates := map[int64]struct{}{}

	for _, r := range merged {
		key := rowIdentity(r)
		if c, bad := conflicts[key]; bad {
			plan.Rows = append(plan.Rows, &ImportPlanRow{Index: len(plan.Rows), Row: r, Action: "conflict",
				Reason: "文件内同身份字段值冲突: " + strings.Join(c, ",") + "（首见 " + r.Sheet + " 第 " + itoaImport(r.Line) + " 行）"})
			plan.Summary.Conflict++
			continue
		}
		if amb := ambiguous[key]; len(amb) > 0 {
			plan.Rows = append(plan.Rows, &ImportPlanRow{Index: len(plan.Rows), Row: r, Action: "conflict",
				Reason: fmt.Sprintf("库内存在 %d 条同身份旧行（IP 等价写法），无法唯一匹配", len(amb)+1)})
			plan.Summary.Conflict++
			continue
		}
		cur, found := existing[key]
		if !found {
			plan.Rows = append(plan.Rows, &ImportPlanRow{Index: len(plan.Rows), Row: r, Action: "create"})
			plan.Summary.Create++
			continue
		}
		// 企业归属冲突（D10）
		if opts.CompanyID > 0 && cur.CompanyID != nil && *cur.CompanyID != opts.CompanyID && opts.CompanyPolicy != "keep" {
			plan.Rows = append(plan.Rows, &ImportPlanRow{Index: len(plan.Rows), Row: r, Action: "conflict", AssetID: cur.ID,
				Reason: fmt.Sprintf("已有企业归属 #%d 与所选企业 #%d 冲突；可选择「保留原企业」继续导入其它信息", *cur.CompanyID, opts.CompanyID)})
			plan.Summary.Conflict++
			continue
		}
		diffs, updates, hasFill, hasChange := mergeDiffs(r, cur, opts)
		action := "unchanged"
		switch {
		case hasChange:
			action = "overwrite"
			plan.Summary.Overwrite++
			updateCandidates[cur.ID] = struct{}{}
		case hasFill:
			action = "fill"
			plan.Summary.Fill++
			updateCandidates[cur.ID] = struct{}{}
		default:
			plan.Summary.Unchanged++
		}
		row := &ImportPlanRow{Index: len(plan.Rows), Row: r, Action: action, AssetID: cur.ID, Diffs: diffs, updates: updates}
		plan.Rows = append(plan.Rows, row)
	}

	// 共享引用任务（A09）：更新的全局行有哪些任务在引用（直接关联 + 图锚）
	if len(updateCandidates) > 0 {
		ids := make([]int64, 0, len(updateCandidates))
		for id := range updateCandidates {
			ids = append(ids, id)
		}
		shared, err := sharedTaskRefs(q, ids, opts.TaskID)
		if err != nil {
			return nil, err
		}
		sharedLookup = shared
		for _, pr := range plan.Rows {
			if pr.AssetID > 0 {
				if refs := sharedLookup[pr.AssetID]; len(refs) > 0 && (pr.Action == "fill" || pr.Action == "overwrite") {
					pr.SharedTasks = refs
				}
			}
		}
	}
	plan.Summary.Total = len(plan.Rows)
	plan.Hash = planHash(plan, existing, rows, rowErrors, opts)
	return plan, nil
}

// mergeInFile merges same-identity rows in the file. 矛盾标量 → identity 进
// conflicts（值为冲突字段名列表），不隐式最后一行覆盖。
func mergeInFile(rows []*assettransfer.Row) ([]*assettransfer.Row, map[importIdentity][]string) {
	byID := map[importIdentity]*assettransfer.Row{}
	order := make([]importIdentity, 0, len(rows))
	conflictFields := map[importIdentity]map[string]bool{}

	scalarOf := func(r *assettransfer.Row, field string) (string, bool) {
		switch field {
		case "icp":
			return r.ICP, r.ICP != ""
		case "record_type":
			return r.RecordType, r.RecordType != ""
		case "record_value":
			return strings.Join(r.RecordValue, ";"), len(r.RecordValue) > 0
		case "app_description":
			return r.AppDescription, r.AppDescription != ""
		case "app_icp":
			return r.AppICP, r.AppICP != ""
		case "category":
			return r.Category, r.Category != ""
		case "url":
			return r.URL, r.URL != ""
		case "page_title":
			return r.PageTitle, r.PageTitle != ""
		case "favicon_mmh3":
			return r.FaviconMMH3, r.FaviconMMH3 != ""
		case "status_code":
			if r.StatusCode == nil {
				return "", false
			}
			return fmt.Sprint(*r.StatusCode), true
		case "content_length":
			if r.ContentLength == nil {
				return "", false
			}
			return fmt.Sprint(*r.ContentLength), true
		case "service_name":
			return r.ServiceName, r.ServiceName != ""
		case "port":
			if r.Port == 0 {
				return "", false
			}
			return fmt.Sprint(r.Port), true
		case "ip":
			return r.IP, r.IP != ""
		case "domain":
			return r.Domain, r.Domain != ""
		case "note":
			return r.Note, r.Note != ""
		}
		return "", false
	}
	scalarFields := []string{"icp", "record_type", "record_value", "app_description", "app_icp", "category",
		"url", "page_title", "favicon_mmh3", "status_code", "content_length", "service_name", "port", "ip", "domain", "note"}

	for _, r := range rows {
		key := rowIdentity(r)
		if conflictFields[key] != nil {
			continue // 已冲突身份：后续行不再并入
		}
		first, ok := byID[key]
		if !ok {
			var cp assettransfer.Row
			raw, _ := json.Marshal(r)
			_ = json.Unmarshal(raw, &cp)
			byID[key] = &cp
			order = append(order, key)
			continue
		}
		// 标量矛盾检测
		var fields []string
		for _, f := range scalarFields {
			a, aok := scalarOf(first, f)
			b, bok := scalarOf(r, f)
			if aok && bok && a != b {
				fields = append(fields, f)
			}
		}
		if len(fields) > 0 {
			conflictFields[key] = map[string]bool{}
			for _, f := range fields {
				conflictFields[key][f] = true
			}
			continue
		}
		// 兼容合并：标量补空、集合并集
		if first.ICP == "" {
			first.ICP = r.ICP
		}
		if first.Note == "" {
			first.Note = r.Note
		}
		if first.IP == "" {
			first.IP = r.IP
		}
		if first.Domain == "" {
			first.Domain = r.Domain
		}
		if len(r.RecordValue) > 0 {
			first.RecordValue = toArray(append(first.RecordValue, r.RecordValue...))
		}
		if len(r.BoundDomains) > 0 {
			first.BoundDomains = toArray(append(first.BoundDomains, r.BoundDomains...))
		}
		if len(r.OpenPorts) > 0 {
			first.OpenPorts = mergePortEntries(first.OpenPorts, r.OpenPorts)
		}
		if len(r.Technologies) > 0 {
			first.Technologies = toArray(append(first.Technologies, r.Technologies...))
		}
		if len(r.Auth) > 0 {
			first.Auth = mergeAuthEntries(first.Auth, r.Auth)
		}
		if len(r.Params) > 0 {
			first.Params = mergeParamEntries(first.Params, r.Params)
		}
		if r.StatusCode != nil && first.StatusCode == nil {
			first.StatusCode = r.StatusCode
		}
		if r.ContentLength != nil && first.ContentLength == nil {
			first.ContentLength = r.ContentLength
		}
		if first.PageTitle == "" {
			first.PageTitle = r.PageTitle
		}
		if first.FaviconMMH3 == "" {
			first.FaviconMMH3 = r.FaviconMMH3
		}
		if first.AppDescription == "" {
			first.AppDescription = r.AppDescription
		}
		if first.Category == "" {
			first.Category = r.Category
		}
		if first.AppICP == "" {
			first.AppICP = r.AppICP
		}
		if first.URL == "" && r.Type == "app" {
			first.URL = r.URL
		}
		for k, v := range r.Extra {
			if _, exists := first.Extra[k]; !exists {
				if first.Extra == nil {
					first.Extra = map[string]any{}
				}
				first.Extra[k] = v
			}
		}
	}
	out := make([]*assettransfer.Row, 0, len(order))
	conflicts := map[importIdentity][]string{}
	for _, key := range order {
		if cf := conflictFields[key]; cf != nil {
			fields := make([]string, 0, len(cf))
			for f := range cf {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			conflicts[key] = fields
			out = append(out, byID[key]) // 保留行用于预览展示冲突
			continue
		}
		out = append(out, byID[key])
	}
	return out, conflicts
}

func mergePortEntries(a, b []assettransfer.PortEntry) []assettransfer.PortEntry {
	seen := map[int]bool{}
	out := make([]assettransfer.PortEntry, 0, len(a)+len(b))
	for _, p := range a {
		seen[p.Port] = true
		out = append(out, p)
	}
	for _, p := range b {
		if !seen[p.Port] {
			out = append(out, p)
		}
	}
	return out
}

func mergeAuthEntries(a, b []assettransfer.AuthEntry) []assettransfer.AuthEntry {
	key := func(e assettransfer.AuthEntry) string {
		return fmt.Sprintf("%s|%s|%s|%s|%s", e.Type, e.Username, e.Password, e.Token, e.Description)
	}
	seen := map[string]bool{}
	out := make([]assettransfer.AuthEntry, 0, len(a)+len(b))
	for _, e := range a {
		seen[key(e)] = true
		out = append(out, e)
	}
	for _, e := range b {
		if !seen[key(e)] {
			out = append(out, e)
		}
	}
	return out
}

func mergeParamEntries(a, b []assettransfer.ParamEntry) []assettransfer.ParamEntry {
	key := func(e assettransfer.ParamEntry) string {
		return strings.ToLower(e.Location) + "|" + strings.ToLower(e.Name)
	}
	seen := map[string]bool{}
	out := make([]assettransfer.ParamEntry, 0, len(a)+len(b))
	for _, e := range a {
		seen[key(e)] = true
		out = append(out, e)
	}
	for _, e := range b {
		if !seen[key(e)] {
			out = append(out, e)
		}
	}
	return out
}

// mergeDiffs computes field diffs for one matched row. fill 模式：只补空/新增
// 集合成员；同字段已有不同值显示 kind=keep（不覆盖，仅提示）。overwrite 模式：
// 明确提供且非空的允许字段会改变（change/replace）。空白一律不清空（A05）。
func mergeDiffs(r *assettransfer.Row, cur *Asset, opts assettransfer.Options) (diffs []ImportDiff, updates map[string]any, hasFill, hasChange bool) {
	updates = map[string]any{}
	overwrite := opts.Conflict == "overwrite"
	empty := func(v any) bool { return v == nil || marshalCompactImport(v) == `""` }
	field := func(name string, incoming, old any, sensitive bool) {
		if empty(incoming) {
			return
		}
		if marshalCompactImport(incoming) == marshalCompactImport(old) {
			return
		}
		kind := "keep"
		if empty(old) {
			kind = "fill"
			hasFill = true
		} else if overwrite {
			kind = "change"
			hasChange = true
		}
		d := ImportDiff{Field: name, Kind: kind}
		if sensitive {
			d.Note = "保留现有字段（值不回显）"
			if kind != "keep" {
				d.Kind = "mask"
				d.Note = "补充字段（值不回显）"
				if kind == "change" {
					d.Note = "覆盖字段（值不回显）"
				}
			}
		} else {
			if old != nil {
				d.Old = fmt.Sprint(old)
			}
			d.New = fmt.Sprint(incoming)
		}
		diffs = append(diffs, d)
		if kind != "keep" {
			updates[name] = incoming
		}
	}
	scalar := func(name, incoming, old string) {
		var oldValue any
		if old != "" {
			oldValue = old
		}
		field(name, incoming, oldValue, false)
	}
	list := func(name string, incoming, old []string, key func(string) string) {
		if len(incoming) == 0 {
			return
		}
		seen := map[string]bool{}
		supplied := map[string]bool{}
		result := append([]string(nil), old...)
		for _, v := range old {
			seen[key(v)] = true
		}
		adds, removes := 0, 0
		if overwrite {
			result = nil
		}
		for _, v := range incoming {
			k := key(v)
			if supplied[k] {
				continue
			}
			supplied[k] = true
			if !seen[k] {
				adds++
			}
			if overwrite || !seen[k] {
				result = append(result, v)
			}
		}
		if overwrite {
			for k := range seen {
				if !supplied[k] {
					removes++
				}
			}
		}
		if adds == 0 && removes == 0 {
			return
		}
		updates[name] = result
		if adds > 0 {
			hasFill = true
			diffs = append(diffs, ImportDiff{Field: name, Kind: "add", New: fmt.Sprintf("+%d", adds)})
		}
		if removes > 0 {
			hasChange = true
			diffs = append(diffs, ImportDiff{Field: name, Kind: "remove", Old: fmt.Sprintf("-%d", removes), Note: "覆盖模式移除未列出的成员"})
		}
	}

	identity := func(s string) string { return s }
	switch r.Type {
	case "root_domain":
		scalar("icp", r.ICP, cur.ICP)
	case "ip":
		list("bound_domains", r.BoundDomains, cur.BoundDomains, strings.ToLower)
	case "subdomain":
		scalar("icp", r.ICP, cur.ICP)
		scalar("ip", r.IP, cur.IP)
		list("record_value", r.RecordValue, cur.RecordValue, identity)
	case "app":
		scalar("category", r.Category, cur.Category)
		scalar("app_description", r.AppDescription, cur.AppDescription)
		scalar("app_icp", r.AppICP, cur.AppICP)
		scalar("url", r.URL, cur.URL)
	case "service":
		if r.ServiceType == "http" {
			if r.StatusCode != nil {
				var old any
				if cur.StatusCode != nil {
					old = *cur.StatusCode
				}
				field("status_code", *r.StatusCode, old, false)
			}
			if r.ContentLength != nil {
				var old any
				if cur.ContentLength != nil {
					old = *cur.ContentLength
				}
				field("content_length", *r.ContentLength, old, false)
			}
			scalar("page_title", r.PageTitle, cur.PageTitle)
			scalar("favicon_mmh3", r.FaviconMMH3, cur.FaviconMMH3)
			list("technologies", r.Technologies, cur.Technologies, strings.ToLower)
		} else {
			scalar("domain", r.Domain, cur.Domain)
		}
		scalar("ip", r.IP, cur.IP)
	case "endpoint":
		scalar("ip", r.IP, cur.IP)
	}
	if opts.CompanyID > 0 && cur.CompanyID == nil {
		field("company_id", opts.CompanyID, nil, false)
		updates["company_source"] = "explicit"
	}
	// Structured collections retain old members and merge explicitly provided subfields.
	// Authentication uses (type, username) when available; token-only entries use
	// their complete value to avoid collapsing unrelated credentials.
	structured := func(name string, incoming any, old []map[string]any, key func(map[string]any) string) {
		b, _ := json.Marshal(incoming)
		var entries []map[string]any
		_ = json.Unmarshal(b, &entries)
		if len(entries) == 0 {
			return
		}
		result := make([]map[string]any, 0, len(old)+len(entries))
		positions := map[string]int{}
		for _, e := range old {
			cp := map[string]any{}
			for k, v := range e {
				cp[k] = v
			}
			positions[key(e)] = len(result)
			result = append(result, cp)
		}
		changed, filled := false, false
		for _, e := range entries {
			pos, ok := positions[key(e)]
			if !ok {
				positions[key(e)] = len(result)
				result = append(result, e)
				filled = true
				continue
			}
			target := result[pos]
			for k, v := range e {
				if empty(v) || marshalCompactImport(v) == marshalCompactImport(target[k]) {
					continue
				}
				if empty(target[k]) {
					target[k] = v
					filled = true
				} else if overwrite {
					target[k] = v
					changed = true
				}
			}
		}
		if !filled && !changed {
			return
		}
		updates[name] = result
		hasFill = hasFill || filled
		hasChange = hasChange || changed
		kind, note := "mask", "补充结构化字段（值不回显；保留已有成员）"
		if changed {
			note = "覆盖结构化子字段（值不回显；保留已有成员）"
		}
		diffs = append(diffs, ImportDiff{Field: name, Kind: kind, Note: note})
	}
	if r.Type == "ip" {
		structured("open_ports", r.OpenPorts, cur.OpenPorts, func(e map[string]any) string { return fmt.Sprint(e["port"]) })
	}
	if overwrite && len(r.Auth) > 0 {
		b, _ := json.Marshal(r.Auth)
		var entries []map[string]any
		_ = json.Unmarshal(b, &entries)
		if marshalCompactImport(entries) != marshalCompactImport(cur.Auth) {
			updates["auth"] = entries
			hasChange = true
			diffs = append(diffs, ImportDiff{Field: "auth", Kind: "mask", Note: fmt.Sprintf("覆盖为 %d 条认证（现有 %d 条；值不回显）", len(entries), len(cur.Auth))})
		}
	} else {
		structured("auth", r.Auth, cur.Auth, func(e map[string]any) string {
			typ, _ := e["type"].(string)
			user, _ := e["username"].(string)
			if user != "" {
				return typ + "|" + user
			}
			return marshalCompactImport(e)
		})
	}

	structured("params", r.Params, cur.Params, func(e map[string]any) string {
		loc, _ := e["location"].(string)
		name, _ := e["name"].(string)
		return strings.ToLower(loc) + "|" + strings.ToLower(name)
	})
	extra := map[string]any{}
	for k, v := range cur.Extra {
		extra[k] = v
	}
	incoming := map[string]any{}
	for k, v := range r.Extra {
		incoming[k] = v
	}
	if r.Note != "" {
		incoming["note"] = r.Note
	}
	keys := make([]string, 0, len(incoming))
	for k := range incoming {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := "extra." + k
		field(name, incoming[k], cur.Extra[k], true)
		if v, ok := updates[name]; ok {
			extra[k] = v
			delete(updates, name)
			updates["extra"] = extra
		}
	}
	return
}

func anyToInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case json.Number:
		n, err := x.Int64()
		return int(n), err
	}
	return 0, fmt.Errorf("not a number")
}

func marshalCompactImport(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func itoaImport(n int) string { return fmt.Sprint(n) }

// sharedTaskRefs returns other live tasks referencing the assets（直接关联 +
// 图锚；A09）。excludedTaskID（导入目标任务）不计入"其它"。
func sharedTaskRefs(q importQuerier, assetIDs []int64, excludedTaskID int64) (map[int64][]int64, error) {
	rows, err := q.Query(`
SELECT a.id, t.id
FROM assets a
JOIN tasks t ON (t.id = ANY(a.task_ids) OR EXISTS (
    SELECT 1 FROM exploration_anchors ea
    JOIN exploration_nodes n ON n.id = ea.node_id
    WHERE ea.asset_id = a.id AND n.exploration_id = t.exploration_id
)) AND t.deleted_at IS NULL AND t.id <> $2
WHERE a.id = ANY($1::bigint[])`, toArrayInt64(assetIDs), excludedTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var assetID, taskID int64
		if err := rows.Scan(&assetID, &taskID); err != nil {
			return nil, err
		}
		out[assetID] = appendUniqInt64(out[assetID], taskID)
	}
	return out, rows.Err()
}

func toArrayInt64(in []int64) []int64 {
	seen := map[int64]struct{}{}
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func appendUniqInt64(list []int64, v int64) []int64 {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// planHash binds preview to commit (A12/A13)：行计划 + 命中资产的完整快照。
func planHash(plan *ImportPlan, existing map[importIdentity]*Asset, rows []*assettransfer.Row, rowErrors []*assettransfer.RowError, opts assettransfer.Options) string {
	// JSON supplies unambiguous framing and stable map-key ordering. Bind raw input,
	// normalized options, masked values, computed writes, and full matched snapshots.
	snapshots := make([]*Asset, 0, len(existing))
	for _, a := range existing {
		snapshots = append(snapshots, a)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	for _, pr := range plan.Rows {
		sort.Slice(pr.SharedTasks, func(i, j int) bool { return pr.SharedTasks[i] < pr.SharedTasks[j] })
	}
	writes := make([]map[string]any, len(plan.Rows))
	for i, pr := range plan.Rows {
		writes[i] = pr.updates
	}
	payload := struct {
		Rows      []*assettransfer.Row
		Errors    []*assettransfer.RowError
		Options   assettransfer.Options
		Plan      []*ImportPlanRow
		Writes    []map[string]any
		Snapshots []*Asset
	}{rows, rowErrors, opts.Normalized(), plan.Rows, writes, snapshots}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
