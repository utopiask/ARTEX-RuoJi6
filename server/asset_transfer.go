package server

// 资产导入导出 HTTP 端点（实施方案 §12）。四步导入闭环：模板/上传粘贴 →
// 预览 → 确认提交 → 结果与错误清单；导出按明确范围出 XLSX/CSV/JSON，超限
// 分包 ZIP。错误语义：400/401/403/404/409/410/413/422/500。

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/assettransfer"
	"github.com/Autumn-27/artex/db"
)

const maxImportPreviewBody = assettransfer.MaxUploadBytes + 1<<20 // 文件 + 表单余量

// =====================================================================
// GET /api/assets/import-template?format=xlsx|csv
// =====================================================================

func (s *Server) assetImportTemplate(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("format") {
	case "", "xlsx":
		data, err := assettransfer.TemplateXLSX()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		setAssetDownload(w, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "artex-资产导入模板.xlsx")
		_, _ = w.Write(data)
	case "csv":
		setAssetDownload(w, "text/csv; charset=utf-8", "artex-资产导入模板.csv")
		_, _ = w.Write(assettransfer.TemplateCSV())
	default:
		writeErr(w, 400, "bad format: "+r.URL.Query().Get("format"))
	}
}

func setAssetDownload(w http.ResponseWriter, contentType, filename string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, sanitizeFilename(filename), url.PathEscape(filename)))
}

// =====================================================================
// POST /api/assets/imports/preview   (multipart: file 或 text + options JSON)
// =====================================================================

func (s *Server) assetImportPreview(w http.ResponseWriter, r *http.Request) {
	if s.assetStore() == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportPreviewBody)
	if err := r.ParseMultipartForm(assettransfer.MaxUploadBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("上传数据超过 %d MiB 上限", assettransfer.MaxUploadBytes>>20))
			return
		}
		writeErr(w, 400, "invalid multipart form: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	var opts assettransfer.Options
	if raw := r.FormValue("options"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			writeErr(w, 400, "invalid options JSON: "+err.Error())
			return
		}
	}
	opts = opts.Normalized()

	var fileName string
	var data []byte
	if file, header, err := r.FormFile("file"); err == nil {
		defer file.Close()
		if header.Size > assettransfer.MaxUploadBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("上传数据超过 %d MiB 上限", assettransfer.MaxUploadBytes>>20))
			return
		}
		buf, err := io.ReadAll(io.LimitReader(file, assettransfer.MaxUploadBytes+1))
		if err != nil {
			writeErr(w, 400, "read file: "+err.Error())
			return
		}
		if len(buf) > assettransfer.MaxUploadBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("上传数据超过 %d MiB 上限", assettransfer.MaxUploadBytes>>20))
			return
		}
		fileName, data = header.Filename, buf
	} else if !errors.Is(err, http.ErrMissingFile) {
		writeErr(w, 400, "read file field: "+err.Error())
		return
	}
	pasted := strings.TrimSpace(r.FormValue("text"))
	if len(pasted) > assettransfer.MaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "粘贴内容超过上传大小限制")
		return
	}
	if len(data) == 0 && pasted == "" {
		writeErr(w, 400, "缺少上传文件或粘贴文本")
		return
	}
	if len(data) > 0 && pasted != "" {
		writeErr(w, 400, "file 与 text 只能提供其一")
		return
	}

	var parsed *assettransfer.ParseResult
	var perr error
	if len(data) > 0 {
		parsed, perr = assettransfer.Parse(fileName, data, "")
	} else {
		// 粘贴文本：{ 开头按 JSON 包处理，否则 TXT 规则
		if strings.HasPrefix(pasted, "{") {
			parsed, perr = assettransfer.ParseJSON([]byte(pasted))
		} else {
			parsed = assettransfer.ParseTXT(pasted)
		}
	}
	if perr != nil {
		var pe *assettransfer.ParseError
		if errors.As(perr, &pe) {
			writeErr(w, pe.Code, pe.Message)
			return
		}
		writeErr(w, 500, perr.Error())
		return
	}
	totalRows := len(parsed.Rows) + len(parsed.Errors)
	if totalRows > assettransfer.MaxRows {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("单批最多 %d 行（解析出 %d 行），请拆分文件", assettransfer.MaxRows, totalRows))
		return
	}

	// 校验归属目标（不自动创建，§8）
	if opts.CompanyID > 0 {
		var exists bool
		if err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM companies WHERE id=$1)`, opts.CompanyID).Scan(&exists); err == nil && !exists {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("企业 #%d 不存在（不自动创建）", opts.CompanyID))
			return
		}
	}
	if opts.TaskID > 0 {
		var exists bool
		if err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL)`, opts.TaskID).Scan(&exists); err == nil && !exists {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("任务 #%d 不存在或已删除（不自动恢复）", opts.TaskID))
			return
		}
	}

	plan, err := db.ComputeImportPlan(s.m.pg, parsed.Rows, parsed.Errors, opts)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sha := sha256.Sum256(data)
	if len(data) == 0 {
		sha = sha256.Sum256([]byte(pasted))
	}
	batchID, err := s.m.pg.CreateImportBatch(fileName, parsed.Format, hex.EncodeToString(sha[:]), opts, parsed, plan)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 顺手清理过期批次（§13），失败不影响本次预览
	_, _, _ = s.m.pg.CleanupImportBatches()

	writeJSON(w, 200, map[string]any{
		"batch_id":    batchID,
		"format":      parsed.Format,
		"file_sha256": hex.EncodeToString(sha[:]),
		"options":     opts,
		"summary":     plan.Summary,
		"plan_hash":   plan.Hash,
		"warnings":    parsed.Warnings,
		"rows_total":  len(plan.Rows),
		"expires_at":  time.Now().Add(assettransfer.PreviewTTL).Format(time.RFC3339),
	})
}

// =====================================================================
// GET /api/assets/imports/{id}?offset=&limit=&action=
// =====================================================================

func (s *Server) assetImportGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid batch id")
		return
	}
	batch, err := s.m.pg.AssetImportBatchByID(id)
	if err != nil {
		writeAssetImportErr(w, err)
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	action := q.Get("action")
	rows := batch.Plan.Rows
	if action != "" {
		filtered := rows[:0:0]
		for _, pr := range rows {
			if pr.Action == action {
				filtered = append(filtered, pr)
			}
		}
		rows = filtered
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	page := rows[offset:end]
	out := map[string]any{
		"batch_id":    batch.ID,
		"status":      batch.Status,
		"file_name":   batch.FileName,
		"file_format": batch.FileFormat,
		"options":     batch.Options,
		"summary":     batch.Plan.Summary,
		"plan_hash":   batch.Plan.Hash,
		"row_errors":  batch.RowErrors,
		"warnings":    batch.Warnings,
		"rows_total":  len(rows),
		"offset":      offset,
		"limit":       limit,
	}
	if len(page) > 0 {
		out["rows"] = page
	} else {
		out["rows"] = []*db.ImportPlanRow{}
	}
	if batch.Receipt != nil {
		out["receipt"] = batch.Receipt
	}
	writeJSON(w, 200, out)
}

// =====================================================================
// POST /api/assets/imports/{id}/commit
// =====================================================================

func (s *Server) assetImportCommit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid batch id")
		return
	}
	var req struct {
		PlanHash string `json:"plan_hash"`
		Rows     []int  `json:"rows"` // 空/缺省 = 全部有效行
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if req.PlanHash == "" {
		writeErr(w, 400, "plan_hash required（提交内容须绑定预览）")
		return
	}
	receipt, err := s.m.pg.CommitImportBatch(id, req.PlanHash, req.Rows)
	if err != nil {
		var done *db.ErrImportAlreadyCommitted
		if errors.As(err, &done) {
			writeJSON(w, 200, map[string]any{"receipt": done.Receipt, "already_committed": true})
			return
		}
		writeAssetImportErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"receipt": receipt})
}

func writeAssetImportErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrImportBatchNotFound):
		writeErr(w, http.StatusNotFound, "导入批次不存在")
	case errors.Is(err, db.ErrImportBatchExpired):
		writeErr(w, http.StatusGone, "导入批次已过期，请重新上传预览")
	case errors.Is(err, db.ErrImportBatchStale):
		writeErr(w, http.StatusConflict, "预览后数据已变化，请重新预览确认")
	case errors.Is(err, db.ErrImportCompanyNotFound):
		writeErr(w, http.StatusNotFound, "所选企业不存在（不自动创建）")
	case errors.Is(err, db.ErrImportTaskNotFound):
		writeErr(w, http.StatusNotFound, "所选任务不存在（不自动创建）")
	case errors.Is(err, db.ErrImportTaskDeleted):
		writeErr(w, http.StatusConflict, "所选任务已删除，不能关联（不自动恢复）")
	default:
		writeErr(w, 500, err.Error())
	}
}

// =====================================================================
// GET /api/assets/imports/{id}/errors
// =====================================================================

func (s *Server) assetImportErrors(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid batch id")
		return
	}
	batch, err := s.m.pg.AssetImportBatchByID(id)
	if err != nil {
		writeAssetImportErr(w, err)
		return
	}
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	buf.WriteString("工作表,行号,类型,对象,字段,原因\n")
	esc := func(v string) string {
		if strings.ContainsAny(v, ",\"\n\r") {
			return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
		}
		return v
	}
	for _, e := range batch.RowErrors {
		fmt.Fprintf(&buf, "%s,%d,%s,%s,%s,%s\n", esc(e.Sheet), e.Line, esc(e.Type), esc(e.Ref), esc(e.Field), esc(e.Reason))
	}
	for _, pr := range batch.Plan.Rows {
		if pr.Action == "conflict" {
			fmt.Fprintf(&buf, "%s,%d,%s,%s,%s,%s\n", esc(pr.Row.Sheet), pr.Row.Line, esc(pr.Row.Type), esc(truncateRef(pr.Row.Label())), "", esc(pr.Reason))
		}
	}
	setAssetDownload(w, "text/csv; charset=utf-8", fmt.Sprintf("artex-导入错误清单-%d.csv", id))
	_, _ = w.Write(buf.Bytes())
}

func truncateRef(v string) string {
	const max = 80
	if len(v) > max {
		return v[:max] + "…"
	}
	return v
}

// =====================================================================
// POST /api/assets/imports/{id}/probe
// =====================================================================

func (s *Server) assetImportProbe(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid batch id")
		return
	}
	batch, err := s.m.pg.AssetImportBatchByID(id)
	if err != nil {
		writeAssetImportErr(w, err)
		return
	}
	if batch.Status != "committed" || batch.Receipt == nil {
		writeErr(w, http.StatusConflict, "只有已提交批次才能排队补全")
		return
	}
	engine := s.m.Enrich()
	if engine == nil {
		writeErr(w, http.StatusServiceUnavailable, "补全引擎不可用")
		return
	}
	ids := append(append([]int64{}, batch.Receipt.CreatedIDs...), batch.Receipt.UpdatedIDs...)
	if len(ids) == 0 {
		writeErr(w, 400, "批次没有可补全的资产")
		return
	}
	assets, err := s.assetStore().GetByIDs(ids)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	allowed, skipped, err := s.allowedAssetImportProbes(batch, assets)
	if err != nil {
		writeErr(w, http.StatusConflict, "补全范围不可用，请检查企业和任务范围后重试")
		return
	}
	queued, dropped := 0, 0
	for _, a := range allowed {
		switch {
		case a.Type == "root_domain" || a.Type == "subdomain":
			if a.Domain == "" {
				skipped++
				continue
			}
			if engine.TryResolveDomain(a.ID, a.Domain) {
				queued++
			} else {
				dropped++
			}
		case a.Type == "service" && a.ServiceType == "http" && a.URL != "":
			if engine.TryProbeSite(a.ID, a.URL) {
				queued++
			} else {
				dropped++
			}
		default:
			skipped++
		}
	}
	writeJSON(w, 200, map[string]any{
		"queued": queued, "not_queued": dropped, "skipped": skipped,
		"note": "仅对已有企业/任务范围内的目标排队；无范围、范围外或不支持类型已跳过。排队不等于完成，未排队部分可重试",
	})
}

// =====================================================================
// POST /api/assets/export
// =====================================================================

type assetExportRequest struct {
	Scope            string  `json:"scope"` // selected|filtered|all
	Format           string  `json:"format"`
	IncludeSensitive bool    `json:"include_sensitive"`
	IDs              []int64 `json:"ids"`
	Filter           struct {
		Type      string `json:"type"`
		CompanyID int64  `json:"company_id"`
		TaskID    int64  `json:"task_id"`
		DSL       string `json:"dsl"`
	} `json:"filter"`
}

func (s *Server) assetExport(w http.ResponseWriter, r *http.Request) {
	as := s.assetStore()
	if as == nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	var req assetExportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	switch req.Format {
	case "xlsx", "csv", "json":
	default:
		writeErr(w, 400, "bad format: "+req.Format)
		return
	}
	var filter db.AssetFilter
	var ids []int64
	scopeDesc := ""
	switch req.Scope {
	case "selected":
		if len(req.IDs) == 0 {
			writeErr(w, 400, "scope=selected 需要 ids")
			return
		}
		if len(req.IDs) > 10000 {
			writeErr(w, http.StatusRequestEntityTooLarge, "勾选导出最多 10000 条")
			return
		}
		ids = req.IDs
		scopeDesc = fmt.Sprintf("勾选 %d 条", len(ids))
	case "filtered":
		filter = db.AssetFilter{Type: req.Filter.Type, CompanyID: req.Filter.CompanyID, TaskID: req.Filter.TaskID, DSL: req.Filter.DSL}
		if filter.DSL != "" {
			if err := db.ValidateDSL(filter.DSL); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
		}
		scopeDesc = "当前筛选"
	case "all":
		scopeDesc = "全库六类资产"
	default:
		writeErr(w, 400, "bad scope: "+req.Scope)
		return
	}

	// 一致性快照流式读取 → 映射 → 分包渲染（§10.1/§10.2）
	renderOpts := assettransfer.ExportOptions{
		Format: req.Format, IncludeSensitive: req.IncludeSensitive,
		GeneratedAt: time.Now(), ScopeDesc: scopeDesc,
	}
	exportAssets := make([]assettransfer.ExportAsset, 0, 1024)
	var mappingErr error
	_, err := as.StreamAssetsForExport(r.Context(), filter, ids, func(batch []*db.Asset) error {
		for _, a := range batch {
			if err := validateAssetExportMapping(a, req.IncludeSensitive); err != nil {
				mappingErr = err
				return err
			}
			exportAssets = append(exportAssets, exportAssetFromDB(a))
		}
		return nil
	})
	if err != nil {
		if mappingErr != nil {
			writeErr(w, http.StatusUnprocessableEntity, mappingErr.Error())
		} else {
			writeErr(w, http.StatusInternalServerError, "导出查询失败")
		}
		return
	}
	if len(exportAssets) == 0 {
		writeErr(w, 404, "没有匹配的资产可导出")
		return
	}
	parts, err := renderAssetExportParts(r.Context(), renderOpts, exportAssets)
	if err != nil {
		// Unsupported cell sizes and sensitive URLs require a user decision,
		// not a truncated download or a misleading internal-server error.
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	stamp := time.Now().Format("20060102-150405")
	if len(parts) == 1 {
		setAssetDownload(w, contentTypeForExt(parts[0].Ext), fmt.Sprintf("artex-assets-%s.%s", stamp, parts[0].Ext))
		_, _ = w.Write(parts[0].Data)
		return
	}
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	for i, part := range parts {
		entry, err := zw.Create(fmt.Sprintf("artex-assets-%s-%d-of-%d.%s", stamp, i+1, len(parts), part.Ext))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := entry.Write(part.Data); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	manifest, err := zw.Create("MANIFEST.txt")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	fmt.Fprintf(manifest, "ARTEX 资产导出包\n格式: %s schema %d\n范围: %s\n总行数: %d\n分包: %d（每包最多 %d 行、%d MiB，并满足工作簿解压限制）\n生成时间: %s\n敏感策略: %s（省略字段: %s）\n",
		assettransfer.FormatName, assettransfer.SchemaVersion, scopeDesc, len(exportAssets), len(parts),
		assettransfer.ExportRowsPerFile, assettransfer.MaxUploadBytes>>20, time.Now().Format("2006-01-02 15:04:05"),
		boolText(req.IncludeSensitive), strings.Join(assettransfer.OmittedFields(req.IncludeSensitive), ","))
	if err := zw.Close(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	setAssetDownload(w, "application/zip", fmt.Sprintf("artex-assets-%s.zip", stamp))
	_, _ = w.Write(zipBuf.Bytes())
}

// exportAssetFromDB maps one db.Asset to the transfer-layer export row.
func exportAssetFromDB(a *db.Asset) assettransfer.ExportAsset {
	e := assettransfer.ExportAsset{
		ID: a.ID, Type: a.Type, CompanyID: a.CompanyID, TaskIDs: a.TaskIDs, LastSeen: a.LastSeen,
		Domain: a.Domain, RootDomain: a.RootDomain, IP: a.IP, CSegment: a.CSegment, ICP: a.ICP,
		RecordType: a.RecordType, RecordValue: a.RecordValue,
		BundleID: a.BundleID, AppName: a.AppName, Category: a.Category,
		AppDescription: a.AppDescription, AppICP: a.AppICP, URL: a.URL,
		ServiceType: a.ServiceType, ServiceName: a.ServiceName, FaviconMMH3: a.FaviconMMH3,
		StatusCode: a.StatusCode, ContentLength: a.ContentLength, PageTitle: a.PageTitle,
		Technologies: a.Technologies, Method: a.Method, Extra: a.Extra,
	}
	if a.Port != nil {
		e.Port = *a.Port
	}
	e.BoundDomains = append(e.BoundDomains, a.BoundDomains...)
	for _, op := range a.OpenPorts {
		port, err := anyToIntExport(op["port"])
		if err != nil {
			continue
		}
		svc, _ := op["service"].(string)
		e.OpenPorts = append(e.OpenPorts, assettransfer.PortEntry{Port: port, Service: svc})
	}
	for _, m := range a.Auth {
		e.Auth = append(e.Auth, assettransfer.AuthEntry{
			Type: strAny(m, "type"), Username: strAny(m, "username"), Password: strAny(m, "password"),
			Token: strAny(m, "token"), Description: strAny(m, "description"),
		})
	}
	for _, m := range a.Params {
		e.Params = append(e.Params, assettransfer.ParamEntry{
			Location: strAny(m, "location"), Name: strAny(m, "name"),
			Value: strAny(m, "value"), Type: strAny(m, "type"),
		})
	}
	return e
}

func strAny(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func anyToIntExport(v any) (int, error) {
	var n int64
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || x != math.Trunc(x) || x < 1 || x > 65535 {
			return 0, fmt.Errorf("invalid integer port")
		}
		n = int64(x)
	case int:
		n = int64(x)
	case int64:
		n = x
	case json.Number:
		var err error
		n, err = x.Int64()
		if err != nil {
			return 0, fmt.Errorf("invalid integer port")
		}
	default:
		return 0, fmt.Errorf("not a number")
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port range")
	}
	return int(n), nil
}

func boolText(b bool) string {
	if b {
		return "包含敏感字段"
	}
	return "默认排除敏感字段"
}

func contentTypeForExt(ext string) string {
	switch ext {
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "csv":
		return "text/csv; charset=utf-8"
	case "json":
		return "application/json; charset=utf-8"
	}
	return "application/octet-stream"
}
