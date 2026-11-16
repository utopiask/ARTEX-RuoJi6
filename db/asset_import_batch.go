package db

// 批次持久化与提交执行（实施方案 §9.2–§9.3）：提交绑定计划哈希、事务内
// 重新校验、有效行同事务写入、回执与写入同事务完成、重复提交返回原回执。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/assettransfer"
)

// Receipt diagnostics retain only structural metadata from fixed allowlists.
// Reasons, references, sheet names, and warning text can contain uploaded secrets.
const importReceiptErrorsSQL = `(SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'sheet', CASE WHEN e->>'sheet' IN ('JSON','CSV','TXT','粘贴文本','根域名','IP','子域名','应用','HTTP服务','其它服务','端点') THEN e->>'sheet' ELSE '上传数据' END,
 'line', CASE WHEN jsonb_typeof(e->'line')='number' THEN e->'line' ELSE '0'::jsonb END,
 'type', CASE WHEN e->>'type' IN ('root_domain','ip','subdomain','app','service','endpoint') THEN e->>'type' ELSE 'unknown' END,
 'field', CASE WHEN e->>'field' IN ('type','domain','ip','url','method','port','service_name','record_type','record_value','icp','bound_domains','open_ports','bundle_id','app_name','category','app_description','app_icp','status_code','content_length','page_title','technologies','favicon_mmh3','auth','params','note','extra') THEN e->>'field' ELSE '' END,
 'ref', CASE WHEN COALESCE(e->>'ref','')='' THEN '' ELSE '[已遮蔽]' END,
 'reason', CASE WHEN COALESCE(e->>'type','') NOT IN ('root_domain','ip','subdomain','app','service','endpoint') OR e->>'reason' LIKE '%未知资产类型%' THEN '未知资产类型（原始内容已隐藏）' ELSE '导入数据无效，请修正后重新预览（原始内容已隐藏）' END
 ) ORDER BY ord), '[]'::jsonb) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(row_errors)='array' THEN row_errors ELSE '[]'::jsonb END) WITH ORDINALITY AS items(e,ord))`

const importReceiptWarningsSQL = `(SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'sheet', CASE WHEN e->>'sheet' IN ('JSON','CSV','TXT','粘贴文本','根域名','IP','子域名','应用','HTTP服务','其它服务','端点') THEN e->>'sheet' ELSE '上传数据' END,
 'detail', '导入警告（原始内容已隐藏）'
 ) ORDER BY ord), '[]'::jsonb) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(warnings)='array' THEN warnings ELSE '[]'::jsonb END) WITH ORDINALITY AS items(e,ord))`

// CreateImportBatch persists a parsed + planned preview batch and returns its id.
func (d *DB) CreateImportBatch(fileName, format, sha string, opts assettransfer.Options, parsed *assettransfer.ParseResult, plan *ImportPlan) (int64, error) {
	rowsJSON, err := json.Marshal(parsed.Rows)
	if err != nil {
		return 0, err
	}
	errsJSON, err := json.Marshal(parsed.Errors)
	if err != nil {
		return 0, err
	}
	warnJSON, err := json.Marshal(parsed.Warnings)
	if err != nil {
		return 0, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return 0, err
	}
	optsJSON, err := json.Marshal(opts.Normalized())
	if err != nil {
		return 0, err
	}
	summaryJSON, err := json.Marshal(plan.Summary)
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.QueryRow(`
INSERT INTO asset_import_batches(status, file_name, file_format, file_sha256, options, rows, row_errors, warnings, plan_hash, plan, summary, expires_at)
VALUES ('ready', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now() + interval '30 minutes')
RETURNING id`,
		fileName, format, sha, string(optsJSON), string(rowsJSON), string(errsJSON), string(warnJSON),
		plan.Hash, string(planJSON), string(summaryJSON)).Scan(&id)
	return id, err
}

// AssetImportBatchByID loads a batch. 过期未清理的 ready 批次按过期处理（§13）。
func (d *DB) AssetImportBatchByID(id int64) (*AssetImportBatch, error) {
	if _, _, err := d.CleanupImportBatches(); err != nil {
		return nil, err
	}
	var b AssetImportBatch
	var optsRaw, rowsRaw, errsRaw, warnRaw, planRaw, summaryRaw, receiptRaw sql.NullString
	err := d.QueryRow(`
SELECT id, status, file_name, file_format, file_sha256,
       COALESCE(options::text,'{}'), COALESCE(rows::text,'[]'),
       COALESCE(row_errors::text,'[]'), COALESCE(warnings::text,'[]'),
       COALESCE(plan::text,'{}'), COALESCE(summary::text,'{}'), receipt::text,
       created_at, expires_at, committed_at
FROM asset_import_batches WHERE id = $1`, id).Scan(
		&b.ID, &b.Status, &b.FileName, &b.FileFormat, &b.FileSHA,
		&optsRaw, &rowsRaw, &errsRaw, &warnRaw,
		&planRaw, &summaryRaw, &receiptRaw,
		&b.CreatedAt, &b.ExpiresAt, &b.CommittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrImportBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(optsRaw.String), &b.Options)
	_ = json.Unmarshal([]byte(rowsRaw.String), &b.Rows)
	_ = json.Unmarshal([]byte(errsRaw.String), &b.RowErrors)
	_ = json.Unmarshal([]byte(warnRaw.String), &b.Warnings)
	b.Plan = &ImportPlan{}
	_ = json.Unmarshal([]byte(planRaw.String), b.Plan)
	if receiptRaw.Valid && receiptRaw.String != "" {
		var receipt ImportReceipt
		if json.Unmarshal([]byte(receiptRaw.String), &receipt) == nil {
			b.Receipt = &receipt
		}
	}
	if b.Status == "expired" || (b.Status == "ready" && time.Now().After(b.ExpiresAt)) {
		return nil, ErrImportBatchExpired
	}
	return &b, nil
}

// importInsertHook is a test seam for A13（真实中途失败 → 已确认部分全部回滚）。
var importInsertHook func(r *assettransfer.Row) error

// ErrImportAlreadyCommitted carries the original receipt for idempotent commits.
type ErrImportAlreadyCommitted struct{ Receipt *ImportReceipt }

func (e *ErrImportAlreadyCommitted) Error() string { return "import batch already committed" }

// CommitImportBatch executes a confirmed batch. planHash must equal the preview
// hash; selected 行下标必须是有效行（create/fill/overwrite/unchanged），
// 空切片 = 全部有效行（§9.2：无效行不能被客户端改状态绕过）。
func (d *DB) CommitImportBatch(batchID int64, planHash string, selected []int) (*ImportReceipt, error) {
	// Bound total time holding the import/reference locks, not just each query.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// 1. 锁定批次，防重复提交；已提交 → 原回执（幂等，§9.3.7）
	var status, storedHash string
	var receiptRaw sql.NullString
	var expiresAt time.Time
	err = tx.QueryRow(`SELECT status, plan_hash, receipt::text, expires_at FROM asset_import_batches WHERE id=$1 FOR UPDATE`, batchID).
		Scan(&status, &storedHash, &receiptRaw, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrImportBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "committed" {
		var receipt ImportReceipt
		if receiptRaw.Valid && json.Unmarshal([]byte(receiptRaw.String), &receipt) == nil {
			return nil, &ErrImportAlreadyCommitted{Receipt: &receipt}
		}
		return nil, fmt.Errorf("batch %d already committed but receipt unreadable", batchID)
	}
	if status == "expired" {
		return nil, ErrImportBatchExpired
	}
	if status != "ready" {
		return nil, fmt.Errorf("batch %d status %q cannot commit", batchID, status)
	}
	if time.Now().After(expiresAt) {
		if _, err := tx.Exec(`UPDATE asset_import_batches SET status='expired', rows='[]'::jsonb, plan='{}'::jsonb, row_errors='[]'::jsonb, warnings='[]'::jsonb WHERE id=$1`, batchID); err == nil {
			_ = tx.Commit()
		}
		return nil, ErrImportBatchExpired
	}

	// 2. 读取批次内容
	var optsRaw, rowsRaw, errsRaw sql.NullString
	if err := tx.QueryRow(`SELECT options::text, rows::text, row_errors::text FROM asset_import_batches WHERE id=$1`, batchID).
		Scan(&optsRaw, &rowsRaw, &errsRaw); err != nil {
		return nil, err
	}
	var opts assettransfer.Options
	_ = json.Unmarshal([]byte(optsRaw.String), &opts)
	opts = opts.Normalized()
	var rows []*assettransfer.Row
	if err := json.Unmarshal([]byte(rowsRaw.String), &rows); err != nil {
		return nil, fmt.Errorf("batch rows corrupted: %w", err)
	}
	var rowErrors []*assettransfer.RowError
	_ = json.Unmarshal([]byte(errsRaw.String), &rowErrors)

	// Bound lock waits and protect the entire replan/write interval, including
	// direct task_ids UPDATEs and anchor/node/task changes by existing writers.
	if _, err := tx.Exec(`SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '30s'`); err != nil {
		return nil, err
	}
	if err := lockCompanyScopeMutation(tx); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`LOCK TABLE assets, companies, tasks, exploration_nodes, exploration_anchors, task_asset_links IN EXCLUSIVE MODE`); err != nil {
		return nil, err
	}

	// 3. 校验企业与任务（§8：不自动创建；任务须存在且未删除）
	if opts.CompanyID > 0 {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM companies WHERE id=$1)`, opts.CompanyID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrImportCompanyNotFound
		}
	}
	if opts.TaskID > 0 {
		var deleted bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NOT NULL)`, opts.TaskID).Scan(&deleted); err != nil {
			return nil, err
		}
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, opts.TaskID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrImportTaskNotFound
		}
		if deleted {
			return nil, ErrImportTaskDeleted
		}
	}

	// 4. 事务内重算计划；与预览哈希不一致 → 409 重预览（A12）
	plan, err := ComputeImportPlan(tx, rows, rowErrors, opts)
	if err != nil {
		return nil, err
	}
	if plan.Hash != planHash || plan.Hash != storedHash {
		return nil, ErrImportBatchStale
	}

	// 5. 行选择校验
	selectSet := map[int]bool{}
	for _, idx := range selected {
		if idx < 0 || idx >= len(plan.Rows) {
			return nil, fmt.Errorf("selected row index %d out of range", idx)
		}
		switch plan.Rows[idx].Action {
		case "create", "fill", "overwrite", "unchanged":
		default:
			return nil, fmt.Errorf("selected row index %d is %s and cannot be committed", idx, plan.Rows[idx].Action)
		}
		selectSet[idx] = true
	}

	// 6. 应用写入（同事务；意外错误整体回滚，A13）
	scoped := &AssetStore{db: d, company: d.Companies(), tx: tx}
	receipt := &ImportReceipt{BatchID: batchID, CommittedAt: time.Now().Format(time.RFC3339)}
	summary := ImportSummary{}
	var attachIDs []int64
	for _, pr := range plan.Rows {
		if len(selected) > 0 && !selectSet[pr.Index] {
			continue
		}
		switch pr.Action {
		case "create":
			if importInsertHook != nil {
				if herr := importInsertHook(pr.Row); herr != nil {
					return nil, fmt.Errorf("写入 %s 第 %d 行失败: %w", pr.Row.Sheet, pr.Row.Line, herr)
				}
			}
			id, err := insertImportRow(scoped, tx, pr.Row, opts)
			if err != nil {
				return nil, fmt.Errorf("写入 %s 第 %d 行失败: %w", pr.Row.Sheet, pr.Row.Line, err)
			}
			receipt.CreatedIDs = append(receipt.CreatedIDs, id)
			attachIDs = append(attachIDs, id)
			summary.Create++
		case "fill":
			if err := applyImportUpdate(tx, pr, opts); err != nil {
				return nil, fmt.Errorf("更新 %s 第 %d 行（资产 #%d）失败: %w", pr.Row.Sheet, pr.Row.Line, pr.AssetID, err)
			}
			receipt.UpdatedIDs = append(receipt.UpdatedIDs, pr.AssetID)
			attachIDs = append(attachIDs, pr.AssetID)
			summary.Fill++
		case "overwrite":
			if err := applyImportUpdate(tx, pr, opts); err != nil {
				return nil, fmt.Errorf("更新 %s 第 %d 行（资产 #%d）失败: %w", pr.Row.Sheet, pr.Row.Line, pr.AssetID, err)
			}
			receipt.UpdatedIDs = append(receipt.UpdatedIDs, pr.AssetID)
			attachIDs = append(attachIDs, pr.AssetID)
			summary.Overwrite++
		case "unchanged":
			attachIDs = append(attachIDs, pr.AssetID)
			summary.Unchanged++
		}
	}
	summary.Total = summary.Create + summary.Fill + summary.Overwrite + summary.Unchanged

	// 7. 任务关联：复用 AttachAssetsToTask 的语句顺序（UPDATE 触发器在前、
	// 手工来源 INSERT 在后），按 100 分块但同事务（§8）。
	if opts.TaskID > 0 && len(attachIDs) > 0 {
		ids := toArrayInt64(attachIDs)
		var existing int
		if err := tx.QueryRow(`SELECT count(*) FILTER (WHERE $1=ANY(task_ids)) FROM assets WHERE id=ANY($2::bigint[])`, opts.TaskID, ids).Scan(&existing); err != nil {
			return nil, err
		}
		summaryText := fmt.Sprintf("资产导入批次 #%d", batchID)
		for start := 0; start < len(ids); start += MaxTaskAssetMutationCount {
			end := start + MaxTaskAssetMutationCount
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			if _, err := tx.Exec(`
UPDATE assets
SET task_ids=CASE WHEN $1=ANY(task_ids) THEN task_ids ELSE array_append(task_ids,$1) END
WHERE id=ANY($2::bigint[])`, opts.TaskID, chunk); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT $1, id, 'manual', $3 FROM assets WHERE id=ANY($2::bigint[])
ON CONFLICT (task_id, asset_id) DO UPDATE
SET source='manual', source_summary=EXCLUDED.source_summary, source_node_id=NULL`,
				opts.TaskID, chunk, summaryText); err != nil {
				return nil, err
			}
		}
		receipt.Attached = len(ids) - existing
		receipt.Existing = existing
	}
	receipt.Summary = summary

	// 8. 批次状态 + 清理原始上传内容 + 回执（与写入同事务，§9.3.6）
	receiptJSON, _ := json.Marshal(receipt)
	if _, err := tx.Exec(`
UPDATE asset_import_batches
SET status='committed', receipt=$2::jsonb, committed_at=now(),
    rows='[]'::jsonb, plan='{}'::jsonb, row_errors=`+importReceiptErrorsSQL+`, warnings=`+importReceiptWarningsSQL+`, expires_at=now() + interval '7 days'
WHERE id=$1`, batchID, string(receiptJSON)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}

// CleanupImportBatches expires stale previews and purges old receipts (§13)。
func (d *DB) CleanupImportBatches() (expired, purged int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.CleanupImportBatchesContext(ctx)
}

func (d *DB) CleanupImportBatchesContext(ctx context.Context) (expired, purged int64, err error) {
	// Re-sanitize legacy committed diagnostics too; sanitized receipts are stable
	// and subsequent reads must not erase the downloadable error list.
	if _, err = d.ExecContext(ctx, `UPDATE asset_import_batches SET rows='[]'::jsonb, plan='{}'::jsonb,
 row_errors=`+importReceiptErrorsSQL+`, warnings=`+importReceiptWarningsSQL+`
 WHERE status='committed' AND (rows <> '[]'::jsonb OR plan <> '{}'::jsonb
 OR row_errors IS DISTINCT FROM `+importReceiptErrorsSQL+`
 OR warnings IS DISTINCT FROM `+importReceiptWarningsSQL+`)`); err != nil {
		return 0, 0, err
	}
	if _, err = d.ExecContext(ctx, `UPDATE asset_import_batches SET rows='[]'::jsonb, plan='{}'::jsonb, row_errors='[]'::jsonb, warnings='[]'::jsonb WHERE status='expired' AND (rows <> '[]'::jsonb OR plan <> '{}'::jsonb OR row_errors <> '[]'::jsonb OR warnings <> '[]'::jsonb)`); err != nil {
		return 0, 0, err
	}

	res, e := d.ExecContext(ctx, `UPDATE asset_import_batches SET status='expired', rows='[]'::jsonb, plan='{}'::jsonb, row_errors='[]'::jsonb, warnings='[]'::jsonb WHERE status='ready' AND expires_at < now()`)
	if e != nil {
		return 0, 0, e
	}
	expired, _ = res.RowsAffected()
	res, e = d.ExecContext(ctx, `DELETE FROM asset_import_batches WHERE (status='committed' AND committed_at < now() - interval '7 days') OR (status='expired' AND expires_at < now() - interval '7 days')`)
	if e != nil {
		return expired, 0, e
	}
	purged, _ = res.RowsAffected()
	return expired, purged, nil
}

// =====================================================================
// 写入应用：新增（无派生副作用）与受控更新（只写计划字段）
// =====================================================================

// insertImportRow inserts one new asset. 只登记文件明确列出的字段和本地可计算
// 字段（root_domain/c_segment），不触发普通 upsert 的派生创建（§9.3）。
func insertImportRow(as *AssetStore, tx *sql.Tx, r *assettransfer.Row, opts assettransfer.Options) (int64, error) {
	companyID, companySource := resolveImportCompany(as, r, opts)
	taskIDs := "{}"
	if opts.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", opts.TaskID)
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	extra := map[string]any{}
	if r.Note != "" {
		extra["note"] = r.Note
	}
	for k, v := range r.Extra {
		if k == "note" && r.Note != "" {
			continue
		}
		extra[k] = v
	}
	if r.SourceRef != nil {
		extra["import_source"] = map[string]any{
			"asset_id": r.SourceRef.AssetID, "company_id": r.SourceRef.CompanyID,
			"last_seen": r.SourceRef.LastSeen,
		}
	}
	extraJSON := "{}"
	if len(extra) > 0 {
		b, _ := json.Marshal(extra)
		extraJSON = string(b)
	}

	var id int64
	var err error
	switch r.Type {
	case "root_domain":
		err = tx.QueryRow(`
INSERT INTO assets(type, domain, root_domain, icp, company_id, company_source, task_ids, extra)
VALUES ('root_domain', $1, $1, $2, $3, $4, $5::bigint[], $6::jsonb)
RETURNING id`,
			DomainKey(r.Domain), nullable(r.ICP), companyID, companySource, taskIDs, extraJSON).Scan(&id)
	case "ip":
		err = tx.QueryRow(`
INSERT INTO assets(type, ip, c_segment, bound_domains, open_ports, company_id, company_source, task_ids, extra)
VALUES ('ip', $1, $2::cidr, $3::text[], $4::jsonb[], $5, $6, $7::bigint[], $8::jsonb)
RETURNING id`,
			r.IP, nullable(r.CSegment), marshalStringArray(r.BoundDomains), portsToJSONBArray(r.OpenPorts),
			companyID, companySource, taskIDs, extraJSON).Scan(&id)
	case "subdomain":
		root := r.RootDomain
		if root == "" {
			root, _ = RootDomain(r.Domain)
			if root == "" {
				root = r.Domain
			}
		}
		err = tx.QueryRow(`
INSERT INTO assets(type, domain, root_domain, record_type, record_value, ip, c_segment, icp, company_id, company_source, task_ids, extra)
VALUES ('subdomain', $1, $2, $3, $4::text[], $5, $6::cidr, $7, $8, $9, $10::bigint[], $11::jsonb)
RETURNING id`,
			DomainKey(r.Domain), root, nullable(r.RecordType), marshalStringArray(r.RecordValue), nullable(r.IP),
			nullable(r.CSegment), nullable(r.ICP), companyID, companySource, taskIDs, extraJSON).Scan(&id)
	case "app":
		err = tx.QueryRow(`
INSERT INTO assets(type, bundle_id, app_name, category, app_description, app_icp, url, company_id, company_source, task_ids, extra)
VALUES ('app', $1, $2, $3, $4, $5, $6, $7, $8, $9::bigint[], $10::jsonb)
RETURNING id`,
			nullable(r.BundleID), r.AppName, nullable(r.Category), nullable(r.AppDescription), nullable(r.AppICP),
			nullable(r.URL), companyID, companySource, taskIDs, extraJSON).Scan(&id)
	case "service":
		if r.ServiceType == "http" {
			norm := normalizeURL(r.URL)
			domain, port, serviceName := parseURL(norm)
			root, _ := RootDomain(domain)
			if root == "" {
				root = domain
			}
			err = tx.QueryRow(`
INSERT INTO assets(type, url, service_type, service_name, domain, ip, port, root_domain, c_segment,
    favicon_mmh3, status_code, content_length, page_title, technologies, auth, company_id, company_source, task_ids, extra)
VALUES ('service', $1, 'http', $2, $3, $4, $5, $6, $7::cidr, $8, $9, $10, $11, $12::text[], $13::jsonb[], $14, $15, $16::bigint[], $17::jsonb)
RETURNING id`,
				norm, serviceName, nullable(domain), nullable(r.IP), nullableInt(port), root, nullable(r.CSegment), nullable(r.FaviconMMH3),
				r.StatusCode, r.ContentLength, nullable(r.PageTitle), marshalStringArray(r.Technologies),
				authToJSONBArray(r.Auth), companyID, companySource, taskIDs, extraJSON).Scan(&id)
		} else {
			domain := DomainKey(r.Domain)
			root := ""
			if domain != "" {
				root, _ = RootDomain(domain)
				if root == "" {
					root = domain
				}
			}
			err = tx.QueryRow(`
INSERT INTO assets(type, service_type, service_name, domain, ip, port, root_domain, c_segment, auth, company_id, company_source, task_ids, extra)
VALUES ('service', 'other', $1, $2, $3, $4, $5, $6::cidr, $7::jsonb[], $8, $9, $10::bigint[], $11::jsonb)
RETURNING id`,
				strings.ToLower(r.ServiceName), nullable(domain), nullable(r.IP), r.Port, nullable(root), nullable(r.CSegment),
				authToJSONBArray(r.Auth), companyID, companySource, taskIDs, extraJSON).Scan(&id)
		}
	case "endpoint":
		norm := normalizeURL(r.URL)
		domain, port, _ := parseURL(norm)
		root, _ := RootDomain(domain)
		if root == "" {
			root = domain
		}
		err = tx.QueryRow(`
INSERT INTO assets(type, url, method, domain, ip, port, root_domain, params, c_segment, company_id, company_source, task_ids, extra)
VALUES ('endpoint', $1, $2, $3, $4, $5, $6, $7::jsonb[], $8::cidr, $9, $10, $11::bigint[], $12::jsonb)
RETURNING id`,
			norm, r.Method, nullable(domain), nullable(r.IP), nullableInt(port), root,
			paramsToJSONBArray(r.Params), nullable(r.CSegment), companyID, companySource, taskIDs, extraJSON).Scan(&id)
	default:
		return 0, fmt.Errorf("unknown asset type %q", r.Type)
	}
	return id, err
}

// resolveImportCompany: 指定企业 → explicit；否则沿用既有可计算归属（scope），
// 不修改企业范围（§8）。
func resolveImportCompany(as *AssetStore, r *assettransfer.Row, opts assettransfer.Options) (any, string) {
	if opts.CompanyID > 0 {
		return opts.CompanyID, "explicit"
	}
	root := r.RootDomain
	icp := r.ICP
	if r.Type == "app" {
		root = ""
		icp = r.AppICP
	}
	if r.Type == "ip" {
		root = ""
	}
	companyID, err := as.resolveCompanyWithICP(root, r.IP, icp)
	if err != nil || companyID == nil {
		return nil, "scope"
	}
	return *companyID, "scope"
}

func isWordChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func portsToJSONBArray(ps []assettransfer.PortEntry) string {
	items := make([]map[string]any, len(ps))
	for i, p := range ps {
		m := map[string]any{"port": p.Port}
		if p.Service != "" {
			m["service"] = p.Service
		}
		items[i] = m
	}
	s, _ := marshalJSONBArray(items)
	return s
}

func authToJSONBArray(as []assettransfer.AuthEntry) string {
	items := make([]map[string]any, len(as))
	for i, a := range as {
		b, _ := json.Marshal(a)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		items[i] = m
	}
	s, _ := marshalJSONBArray(items)
	return s
}

func paramsToJSONBArray(ps []assettransfer.ParamEntry) string {
	items := make([]map[string]any, len(ps))
	for i, p := range ps {
		b, _ := json.Marshal(p)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		items[i] = m
	}
	s, _ := marshalJSONBArray(items)
	return s
}

// applyImportUpdate applies one planned update with a positional-argument builder.
// The planner supplies exact merged values under the commit locks. The executor
// only serializes those values; it never independently merges the original row.
func applyImportUpdate(tx *sql.Tx, pr *ImportPlanRow, opts assettransfer.Options) error {
	keys := make([]string, 0, len(pr.updates))
	for k := range pr.updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var clauses []string
	var args []any
	for _, k := range keys {
		v := pr.updates[k]
		cast := ""
		switch k {
		case "icp", "ip", "category", "app_description", "app_icp", "url", "page_title", "favicon_mmh3", "domain", "status_code", "content_length", "company_id", "company_source":
		case "bound_domains", "record_value", "technologies":
			v = marshalStringArray(v.([]string))
			cast = "::text[]"
		case "open_ports", "auth", "params":
			var err error
			v, err = marshalJSONBArray(v.([]map[string]any))
			if err != nil {
				return err
			}
			cast = "::jsonb[]"
		case "extra":
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			v = string(b)
			cast = "::jsonb"
		default:
			return fmt.Errorf("unsupported planned import field %q", k)
		}
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf("%s=$%d%s", k, len(args), cast))
	}
	if len(clauses) == 0 {
		return nil
	}
	clauses = append(clauses, "last_seen=now()")
	args = append(args, pr.AssetID)
	_, err := tx.Exec("UPDATE assets SET "+strings.Join(clauses, ", ")+fmt.Sprintf(" WHERE id=$%d", len(args)), args...)
	return err
}
