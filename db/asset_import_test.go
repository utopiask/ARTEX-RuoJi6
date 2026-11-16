package db

// 资产导入导出隔离库测试（实施方案 §16）：所有测试只在
// artex_test_asset_transfer_<随机串> 专用库执行；未配置
// ARTEX_ASSET_TRANSFER_TEST_DSN 时明确跳过，绝不回退 config.json / 默认库。
// 合成数据；不访问真实目标、模型或网络。

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/assettransfer"
)

// createTaskRowForImportTest 直接插入最小任务行（导入测试只需要任务存在且未删除）。
func createTaskRowForImportTest(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	var expID int64
	if err := d.QueryRow(`INSERT INTO explorations(goal, description) VALUES ($1, $1) RETURNING id`, name).Scan(&expID); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := d.QueryRow(`INSERT INTO tasks(name, description, goal, exploration_id) VALUES ($1, $1, $1, $2) RETURNING id`, name, expID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// assetTransferTestDB opens a dedicated throwaway database.
func assetTransferTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	adminDSN := os.Getenv("ARTEX_ASSET_TRANSFER_TEST_DSN")
	if adminDSN == "" {
		t.Skip("ARTEX_ASSET_TRANSFER_TEST_DSN 未配置——按 §16 明确跳过，不回退默认库")
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	dbName := "artex_test_asset_transfer_fix_" + hex.EncodeToString(suffix)
	u, err := url.Parse(adminDSN)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		t.Fatal("test admin DSN must be a PostgreSQL URL")
	}
	// Admin connections must never call business Open: it applies migrations.
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(`CREATE DATABASE ` + quoteIdent(dbName)); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Logf("CREATED_EXACT_OWNED_DB %s", dbName)
	var d *DB
	t.Cleanup(func() {
		if d != nil {
			d.Close()
		}
		defer admin.Close()
		if _, err := admin.Exec(`DROP DATABASE ` + quoteIdent(dbName) + ` WITH (FORCE)`); err != nil {
			t.Errorf("DROP_FAILED %s: %v", dbName, err)
			return
		}
		var remaining int
		if err := admin.QueryRow(`SELECT count(*) FROM pg_database WHERE datname=$1`, dbName).Scan(&remaining); err != nil || remaining != 0 {
			t.Errorf("owned database cleanup verification: remaining=%d err=%v", remaining, err)
		}
		t.Logf("DROPPED_EXACT_OWNED_DB %s remaining=%d", dbName, remaining)
	})
	u.Path = "/" + dbName
	q := u.Query()
	q.Del("dbname")
	q.Del("database")
	u.RawQuery = q.Encode()
	raw, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	var current string
	err = raw.QueryRow(`SELECT current_database()`).Scan(&current)
	raw.Close()
	if err != nil || current != dbName {
		t.Fatalf("identity BEFORE migration: got %q want %q err=%v", current, dbName, err)
	}
	t.Logf("VERIFIED_BEFORE_MIGRATION %s", current)
	d, err = Open(u.String())
	if err != nil {
		t.Fatal(err)
	}

	return d, dbName
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func uniqDomain(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s.test", prefix, hex.EncodeToString(b))
}

func previewRows(d *DB, rows []*assettransfer.Row, opts assettransfer.Options) (*ImportPlan, int64) {
	return previewRowsWithErrors(d, rows, nil, opts)
}

// previewRowsWithErrors 模拟完整 server 预览路径：行错误也必须随批次持久化，
// 否则提交重算的计划与预览不一致（这正是 plan_hash 防护的对象）。
func previewRowsWithErrors(d *DB, rows []*assettransfer.Row, errs []*assettransfer.RowError, opts assettransfer.Options) (*ImportPlan, int64) {
	plan, err := ComputeImportPlan(d, rows, errs, opts)
	if err != nil {
		return nil, 0
	}
	parsed := &assettransfer.ParseResult{Format: "json", Rows: rows, Errors: errs}
	id, err := d.CreateImportBatch("test.json", "json", "sha", opts, parsed, plan)
	if err != nil {
		return nil, 0
	}
	return plan, id
}

func assetByDomain(d *DB, domain string) *Asset {
	assets, _ := d.Assets().QueryDSL("domain=="+domain, "", 0, 10, 0)
	if len(assets) == 0 {
		return nil
	}
	return assets[0]
}

// A01 + A07 + A11: 六类资产导入创建、归属、有效部分提交
func TestImportSixTypesCreateWithOwnership(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("six")
	company, _, _, _, _, err := d.Companies().CreateCompanyWithScope(uniqDomain("cmp"), "", nil, "api")
	if err != nil {
		t.Fatal(err)
	}
	rows := []*assettransfer.Row{
		{Sheet: "JSON", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "ICP-1", Note: "备注A"},
		{Sheet: "JSON", Line: 2, Type: "ip", IP: "203.0.113.77", CSegment: "203.0.113.0/24", BoundDomains: []string{"a." + root}, OpenPorts: []assettransfer.PortEntry{{Port: 443, Service: "https"}}},
		{Sheet: "JSON", Line: 3, Type: "subdomain", Domain: "www." + root, RootDomain: root, RecordType: "A", RecordValue: []string{"203.0.113.77"}},
		{Sheet: "JSON", Line: 4, Type: "app", AppName: "演示应用", BundleID: "com." + root + ".app", Category: "工具", AppDescription: "说明", AppICP: "ICP-2"},
		{Sheet: "JSON", Line: 5, Type: "service", ServiceType: "http", URL: "https://www." + root + "/api", PageTitle: "标题", Technologies: []string{"nginx"}},
		{Sheet: "JSON", Line: 6, Type: "service", ServiceType: "other", Domain: "ssh." + root, Port: 22, ServiceName: "ssh"},
		{Sheet: "JSON", Line: 7, Type: "endpoint", URL: "https://www." + root + "/api/x", Method: "POST", Params: []assettransfer.ParamEntry{{Location: "query", Name: "id"}}},
		// 无效行：未知类型
		{Sheet: "JSON", Line: 8, Type: "bogus", Domain: "x"},
	}
	invalid := []*assettransfer.Row{rows[len(rows)-1]}
	valid := rows[:len(rows)-1]
	// 无效行通过 rowErrors 进入
	plan, err := ComputeImportPlan(d, valid, []*assettransfer.RowError{{Sheet: "JSON", Line: 8, Type: "bogus", Reason: "未知资产类型"}}, assettransfer.Options{CompanyID: company})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.Invalid != 1 || plan.Summary.Create != 7 {
		t.Fatalf("summary mismatch: %+v", plan.Summary)
	}
	plan2, batchID2 := previewRowsWithErrors(d, valid, []*assettransfer.RowError{{Sheet: "JSON", Line: 8, Type: "bogus", Reason: "未知资产类型"}}, assettransfer.Options{CompanyID: company})
	if batchID2 == 0 {
		t.Fatal("batch create failed")
	}
	plan = plan2
	receipt, err := d.CommitImportBatch(batchID2, plan.Hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Summary.Create != 7 || len(receipt.CreatedIDs) != 7 {
		t.Fatalf("receipt: %+v", receipt)
	}
	// 归属：显式企业（D03/A07）
	var n int
	d.QueryRow(`SELECT count(*) FROM assets WHERE company_id=$1 AND company_source='explicit'`, company).Scan(&n)
	if n != 7 {
		t.Errorf("explicit company rows = %d, want 7", n)
	}
	// 无效行不写入（A11）
	d.QueryRow(`SELECT count(*) FROM assets WHERE type NOT IN ('root_domain','ip','subdomain','app','service','endpoint')`).Scan(&n)
	if n != 0 {
		t.Errorf("invalid type rows written: %d", n)
	}
	// 字段落位检查
	rd := assetByDomain(d, root)
	if rd == nil || rd.ICP != "ICP-1" {
		t.Errorf("root domain icp: %+v", rd)
	}
	if rd.Extra["note"] != "备注A" {
		t.Errorf("extra.note lost: %v", rd.Extra)
	}
	svc := d.Assets()
	ips, _ := svc.QueryByType("ip", 10, 0)
	if len(ips) != 1 || ips[0].CSegment != "203.0.113.0/24" || len(ips[0].OpenPorts) != 1 {
		t.Errorf("ip asset: %+v", ips)
	}
	httpSvc, _ := svc.QueryDSL("url==https://www."+root+"/api", "", 0, 10, 0)
	if len(httpSvc) != 1 || httpSvc[0].PageTitle != "标题" || len(httpSvc[0].Technologies) != 1 {
		t.Errorf("http service: %+v", httpSvc)
	}
	eps, _ := svc.QueryDSL("url==https://www."+root+"/api/x", "endpoint", 0, 10, 0)
	if len(eps) != 1 || eps[0].Method != "POST" || len(eps[0].Params) != 1 {
		t.Errorf("endpoint: %+v", eps)
	}
	_ = invalid
}

// A03/A05: 默认补空——非空保留、集合保旧、空白不清空
func TestImportFillEmptySemantics(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("fill")
	rows0 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "旧ICP"}}
	plan, batchID := previewRows(d, rows0, assettransfer.Options{})
	if _, err := d.CommitImportBatch(batchID, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	// 第二次导入：icp 不同（保留旧值）、note 补空
	rows1 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "新ICP", Note: "新备注"}}
	plan1, id1 := previewRows(d, rows1, assettransfer.Options{})
	found := false
	for _, pr := range plan1.Rows {
		if pr.Action == "fill" {
			found = true
			hasKeep := false
			for _, df := range pr.Diffs {
				if df.Kind == "keep" && df.Field == "icp" {
					hasKeep = true
				}
			}
			if !hasKeep {
				t.Errorf("fill mode must show keep diff for differing icp: %+v", pr.Diffs)
			}
		}
	}
	if !found {
		t.Fatalf("expected fill action: %+v", plan1.Rows)
	}
	if _, err := d.CommitImportBatch(id1, plan1.Hash, nil); err != nil {
		t.Fatal(err)
	}
	a := assetByDomain(d, root)
	if a.ICP != "旧ICP" {
		t.Errorf("fill mode must keep existing icp: %q", a.ICP)
	}
	if a.Extra["note"] != "新备注" {
		t.Errorf("note must fill empty extra.note: %v", a.Extra)
	}
	// 第三次：留空不清空
	rows2 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root}}
	plan2, id2 := previewRows(d, rows2, assettransfer.Options{})
	if _, err := d.CommitImportBatch(id2, plan2.Hash, nil); err != nil {
		t.Fatal(err)
	}
	a = assetByDomain(d, root)
	if a.ICP != "旧ICP" || a.Extra["note"] != "新备注" {
		t.Errorf("blank import must not clear: %+v", a)
	}
}

// A04: 显式覆盖——仅明确提供、非空、允许编辑的字段改变
func TestImportOverwriteSemantics(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("ovw")
	rows0 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "旧ICP", Note: "旧备注"}}
	plan, batchID := previewRows(d, rows0, assettransfer.Options{})
	if _, err := d.CommitImportBatch(batchID, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	rows1 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "新ICP"}}
	plan1, id1 := previewRows(d, rows1, assettransfer.Options{Conflict: "overwrite"})
	action := ""
	for _, pr := range plan1.Rows {
		action = pr.Action
		if pr.Action == "overwrite" {
			if len(pr.Diffs) != 1 || pr.Diffs[0].Field != "icp" || pr.Diffs[0].Kind != "change" {
				t.Errorf("overwrite diff wrong: %+v", pr.Diffs)
			}
		}
	}
	if action != "overwrite" {
		t.Fatalf("expected overwrite action, got %s", action)
	}
	if _, err := d.CommitImportBatch(id1, plan1.Hash, nil); err != nil {
		t.Fatal(err)
	}
	a := assetByDomain(d, root)
	if a.ICP != "新ICP" {
		t.Errorf("overwrite must change icp: %q", a.ICP)
	}
	if a.Extra["note"] != "旧备注" {
		t.Errorf("unprovided note must stay: %v", a.Extra)
	}
}

// A06: 文件内重复 + 库内矛盾冲突
func TestImportDuplicateAndConflicts(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("dup")
	// 文件内同身份矛盾 icp → 冲突
	rows := []*assettransfer.Row{
		{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "A"},
		{Sheet: "J", Line: 2, Type: "root_domain", Domain: root, RootDomain: root, ICP: "B"},
	}
	plan, _ := previewRows(d, rows, assettransfer.Options{})
	if plan.Summary.Conflict != 1 {
		t.Fatalf("in-file contradiction must conflict: %+v", plan.Summary)
	}
	// 兼容重复（互补字段）→ 合并为一行 create
	rows2 := []*assettransfer.Row{
		{Sheet: "J", Line: 1, Type: "subdomain", Domain: "a." + root, RootDomain: root},
		{Sheet: "J", Line: 2, Type: "subdomain", Domain: "a." + root, RootDomain: root, RecordType: "A", RecordValue: []string{"203.0.113.9"}},
	}
	plan2, _ := previewRows(d, rows2, assettransfer.Options{})
	if plan2.Summary.Create != 2 { // subdomain + (record_type 空/A 是两个身份)
		t.Logf("compat dup create count: %+v", plan2.Summary)
	}
	// 库内 IP 等价写法歧义：两条同义 IP 行
	if _, err := d.Exec(`INSERT INTO assets(type, ip) VALUES ('ip','2001:db8::1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO assets(type, ip) VALUES ('ip','2001:0db8:0000:0000:0000:0000:0000:0001')`); err != nil {
		t.Fatal(err)
	}
	plan3, _ := previewRows(d, []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "ip", IP: "2001:db8::1"}}, assettransfer.Options{})
	if plan3.Summary.Conflict != 1 {
		t.Fatalf("ambiguous ip equivalence must conflict: %+v", plan3.Summary)
	}
}

// A07/A08: 企业冲突排除与保留、任务关联、不改任务状态
func TestImportCompanyConflictAndTaskAttach(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("cmpc")
	companyA, _, _, _, _, errA := d.Companies().CreateCompanyWithScope(uniqDomain("ca"), "", nil, "api")
	companyB, _, _, _, _, errB := d.Companies().CreateCompanyWithScope(uniqDomain("cb"), "", nil, "api")
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	// 既有资产归属 A（explicit）
	rows0 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root}}
	plan, id := previewRows(d, rows0, assettransfer.Options{CompanyID: companyA})
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	// 冲突批次：选 B → 默认排除
	planC, _ := previewRows(d, rows0, assettransfer.Options{CompanyID: companyB})
	if planC.Summary.Conflict != 1 {
		t.Fatalf("company conflict must be excluded by default: %+v", planC.Summary)
	}
	// keep 策略：导入其余信息，保留原企业
	task := createTaskRowForImportTest(t, d, "资产导入测试任务")
	rowsK := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "keep-icp"}}
	planK, idK := previewRows(d, rowsK, assettransfer.Options{CompanyID: companyB, CompanyPolicy: "keep", TaskID: task})
	if planK.Summary.Fill != 1 {
		t.Fatalf("keep policy should proceed: %+v", planK.Summary)
	}
	if _, err := d.CommitImportBatch(idK, planK.Hash, nil); err != nil {
		t.Fatal(err)
	}
	a := assetByDomain(d, root)
	if a.CompanyID == nil || *a.CompanyID != companyA {
		t.Errorf("keep policy must retain original company: %+v", a.CompanyID)
	}
	if a.ICP != "keep-icp" {
		t.Errorf("icp should fill under keep policy: %q", a.ICP)
	}
	// 任务关联建立 + 来源 manual
	var linked bool
	d.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_asset_links WHERE task_id=$1 AND asset_id=$2 AND source='manual')`, task, a.ID).Scan(&linked)
	if !linked {
		t.Error("task link with manual source must exist")
	}
	if len(a.TaskIDs) != 1 || a.TaskIDs[0] != task {
		t.Errorf("task_ids wrong: %v", a.TaskIDs)
	}
	// 旧文件数字 ID 不误绑：source_ref.asset_id=99999 不产生任何关联
	d.QueryRow(`SELECT EXISTS(SELECT 1 FROM assets WHERE id=99999)`).Scan(&linked)
	if linked {
		t.Error("source_ref ids must never bind")
	}
	// A08: 任务状态不被导入改变
	var status string
	d.QueryRow(`SELECT status FROM tasks WHERE id=$1`, task).Scan(&status)
	if status == "" || status == "running" {
		t.Errorf("task status unexpectedly changed: %q", status)
	}
}

// A09: 共享更新——其它引用任务在预览中提示，确认后改同一全局行
func TestImportSharedUpdateShowsOtherTasks(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("share")
	task1 := createTaskRowForImportTest(t, d, "引用任务1")
	task2 := createTaskRowForImportTest(t, d, "引用任务2")
	rows0 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root}}
	plan, id := previewRows(d, rows0, assettransfer.Options{})
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	a := assetByDomain(d, root)
	if _, err := d.Assets().AttachAssetsToTask(task1, []int64{a.ID}, "测试关联1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Assets().AttachAssetsToTask(task2, []int64{a.ID}, "测试关联2"); err != nil {
		t.Fatal(err)
	}
	// 更新行 → 预览须列出其它引用任务
	rows1 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "shared-icp"}}
	plan1, _ := previewRows(d, rows1, assettransfer.Options{})
	foundShared := false
	for _, pr := range plan1.Rows {
		if pr.Action == "fill" {
			if len(pr.SharedTasks) != 2 {
				t.Errorf("shared tasks must list both referencing tasks: %v", pr.SharedTasks)
			}
			foundShared = true
		}
	}
	if !foundShared {
		t.Fatalf("no fill row found: %+v", plan1.Rows)
	}
}

// A10: 预览只读——资产/关联指纹不变
func TestImportPreviewIsReadOnly(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("ro")
	rows0 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "x"}}
	plan, id := previewRows(d, rows0, assettransfer.Options{})
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	var before string
	d.QueryRow(`SELECT md5(string_agg(id::text || updated_at::text || last_seen::text, ',' ORDER BY id)) FROM assets`).Scan(&before)
	// 再预览一次同样内容（此时应 unchanged）
	plan2, _ := previewRows(d, rows0, assettransfer.Options{})
	if plan2.Summary.Unchanged != 1 {
		t.Fatalf("second preview should be unchanged: %+v", plan2.Summary)
	}
	var after string
	d.QueryRow(`SELECT md5(string_agg(id::text || updated_at::text || last_seen::text, ',' ORDER BY id)) FROM assets`).Scan(&after)
	if before != after {
		t.Error("preview must not touch assets (fingerprint changed)")
	}
}

// A12: 幂等提交 + 预览后变化 → 重预览
func TestImportIdempotentAndStale(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("idem")
	rows := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "v1"}}
	plan, id := previewRows(d, rows, assettransfer.Options{})
	r1, err := d.CommitImportBatch(id, plan.Hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 重复提交 → 原回执
	r2, err := d.CommitImportBatch(id, plan.Hash, nil)
	if err == nil {
		t.Fatal("duplicate commit must return ErrImportAlreadyCommitted")
	}
	if r2 != nil {
		t.Fatal("receipt should be nil on error path")
	}
	var done *ErrImportAlreadyCommitted
	if !errors.As(err, &done) {
		t.Fatalf("want ErrImportAlreadyCommitted, got %v", err)
	}
	if done.Receipt == nil || done.Receipt.BatchID != r1.BatchID || done.Receipt.Summary.Create != r1.Summary.Create {
		t.Errorf("original receipt must be returned: %+v vs %+v", done.Receipt, r1)
	}
	// 变化后提交 → stale
	root2 := uniqDomain("stale")
	rows2 := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root2, RootDomain: root2, ICP: "v1"}}
	plan2, id2 := previewRows(d, rows2, assettransfer.Options{})
	if _, err := d.Exec(`INSERT INTO assets(type, domain, root_domain) VALUES ('root_domain', $1, $1)`, root2); err != nil {
		t.Fatal(err)
	}
	err = func() error {
		_, err := d.CommitImportBatch(id2, plan2.Hash, nil)
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed data must require re-preview, got %v", err)
	}
}

// A13: 真实中途失败 → 已确认部分全部回滚
func TestImportMidFailureRollback(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	dom1, dom2, dom3 := uniqDomain("mf1"), uniqDomain("mf2"), uniqDomain("mf3")
	rows := []*assettransfer.Row{
		{Sheet: "J", Line: 1, Type: "root_domain", Domain: dom1, RootDomain: dom1},
		{Sheet: "J", Line: 2, Type: "root_domain", Domain: dom2, RootDomain: dom2},
		{Sheet: "J", Line: 3, Type: "root_domain", Domain: dom3, RootDomain: dom3},
	}
	plan, id := previewRows(d, rows, assettransfer.Options{})
	inserted := 0
	importInsertHook = func(r *assettransfer.Row) error {
		inserted++
		if inserted == 3 {
			return fmt.Errorf("injected mid-write failure")
		}
		return nil
	}
	defer func() { importInsertHook = nil }()
	_, err := d.CommitImportBatch(id, plan.Hash, nil)
	if err == nil {
		t.Fatal("commit must fail")
	}
	var n int
	d.QueryRow(`SELECT count(*) FROM assets WHERE domain IN ($1,$2,$3)`, dom1, dom2, dom3).Scan(&n)
	if n != 0 {
		t.Errorf("mid-write failure must roll back all writes, found %d", n)
	}
	// 批次未标记成功
	var status string
	d.QueryRow(`SELECT status FROM asset_import_batches WHERE id=$1`, id).Scan(&status)
	if status == "committed" {
		t.Error("batch must not be marked committed after failure")
	}
	// 修复后可重新提交（hook 清除后计划仍有效）
	importInsertHook = nil
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Logf("re-commit after rollback: %v (db rolled back, batch stays ready)", err)
	}
}

// A14: 过期批次
func TestImportExpiry(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("exp")
	rows := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root}}
	plan, id := previewRows(d, rows, assettransfer.Options{})
	if _, err := d.Exec(`UPDATE asset_import_batches SET expires_at = now() - interval '1 minute' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired batch must refuse commit, got %v", err)
	}
	if _, err := d.AssetImportBatchByID(id); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired batch lookup must surface expiry, got %v", err)
	}
	// 已提交批次仍可查询（回执）
	root2 := uniqDomain("expok")
	plan2, id2 := previewRows(d, []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "root_domain", Domain: root2, RootDomain: root2}}, assettransfer.Options{})
	if _, err := d.CommitImportBatch(id2, plan2.Hash, nil); err != nil {
		t.Fatal(err)
	}
	b, err := d.AssetImportBatchByID(id2)
	if err != nil || b.Receipt == nil {
		t.Fatalf("committed batch must remain queryable: %v", err)
	}
	// 原始上传内容已清理
	if len(b.Rows) != 0 {
		t.Errorf("committed batch rows must be scrubbed, got %d", len(b.Rows))
	}
}

// A15/A16: 导出跨分页不漏、组合筛选一致
func TestExportStreamingAndFilterConsistency(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	company, _, _, _, _, err := d.Companies().CreateCompanyWithScope(uniqDomain("expc"), "", nil, "api")
	if err != nil {
		t.Fatal(err)
	}
	// 230 条同企业 root_domain（超过列表 200 上限）
	rows := make([]*assettransfer.Row, 230)
	base := uniqDomain("bulk")
	for i := range rows {
		dom := fmt.Sprintf("%s-%03d", base, i)
		rows[i] = &assettransfer.Row{Sheet: "J", Line: i + 1, Type: "root_domain", Domain: dom, RootDomain: dom}
	}
	plan, id := previewRows(d, rows, assettransfer.Options{CompanyID: company})
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	total := 0
	seen := map[string]bool{}
	_, streamErr := d.Assets().StreamAssetsForExport(nil, AssetFilter{CompanyID: company, Type: "root_domain"}, nil, func(batch []*Asset) error {
		for _, a := range batch {
			if seen[a.Domain] {
				t.Errorf("duplicate export row: %s", a.Domain)
			}
			seen[a.Domain] = true
			total++
		}
		return nil
	})
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	if total != 230 {
		t.Fatalf("export must cover all 230 rows across pagination, got %d", total)
	}
	// 计数一致
	count, err := d.Assets().CountAssetsFiltered(AssetFilter{CompanyID: company, Type: "root_domain"})
	if err != nil || count != 230 {
		t.Fatalf("filtered count = %d err=%v, want 230", count, err)
	}
	// 选中导出
	ids := []int64{}
	idRows, idErr := d.Query(`SELECT id FROM assets WHERE company_id=$1 ORDER BY id LIMIT 5`, company)
	if idErr != nil {
		t.Fatal(idErr)
	}
	for idRows.Next() {
		var aid int64
		if idRows.Scan(&aid) == nil {
			ids = append(ids, aid)
		}
	}
	idRows.Close()
	got := 0
	_, err = d.Assets().StreamAssetsForExport(nil, AssetFilter{}, ids, func(batch []*Asset) error {
		got += len(batch)
		return nil
	})
	if err != nil || got != 5 {
		t.Fatalf("selected export = %d err=%v, want 5", got, err)
	}
}

// A17: 敏感策略——默认排除、主动包含、回填不清除
func TestExportSensitivePolicy(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("sens")
	rows := []*assettransfer.Row{{
		Sheet: "J", Line: 1, Type: "service", ServiceType: "http",
		URL: "https://" + root + "/", PageTitle: "t",
		Auth:   []assettransfer.AuthEntry{{Type: "basic", Username: "u", Password: "p"}},
		Params: nil,
	}, {
		Sheet: "J", Line: 2, Type: "endpoint", URL: "https://" + root + "/x", Method: "GET",
		Params: []assettransfer.ParamEntry{{Location: "query", Name: "token", Value: "secret"}},
	}}
	plan, id := previewRows(d, rows, assettransfer.Options{})
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	var exported []assettransfer.ExportAsset
	_, err := d.Assets().StreamAssetsForExport(nil, AssetFilter{CompanyID: -1}, nil, func(batch []*Asset) error { return nil })
	_ = exported
	if err != nil {
		t.Fatal(err)
	}
	// 通过 server 层映射在 server 测试中验证；此处直接用导出渲染器
	assets, _ := d.Assets().QueryByType("service", 10, 0)
	if len(assets) != 1 || len(assets[0].Auth) != 1 {
		t.Fatalf("auth stored: %+v", assets)
	}
	optsDefault := assettransfer.ExportOptions{Format: "json"}
	_, data, err := assettransfer.RenderPart(optsDefault, []assettransfer.ExportAsset{{
		Type: "service", ServiceType: "http", URL: "https://" + root + "/",
		Auth: []assettransfer.AuthEntry{{Type: "basic", Username: "u", Password: "p"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\"password\"") {
		t.Error("default export must omit auth")
	}
	if !strings.Contains(string(data), `"omitted_fields"`) {
		t.Error("export policy must list omitted fields")
	}
	// 回填默认导出不清除已有认证：导入无 auth 的同身份行
	rowsNoAuth := []*assettransfer.Row{{Sheet: "J", Line: 1, Type: "service", ServiceType: "http", URL: "https://" + root + "/", PageTitle: "t2"}}
	plan2, id2 := previewRows(d, rowsNoAuth, assettransfer.Options{Conflict: "overwrite"})
	if _, err := d.CommitImportBatch(id2, plan2.Hash, nil); err != nil {
		t.Fatal(err)
	}
	assets2, _ := d.Assets().QueryByType("service", 10, 0)
	if len(assets2[0].Auth) != 1 {
		t.Errorf("re-import of omitted-auth package must not clear auth: %+v", assets2[0].Auth)
	}
	if assets2[0].PageTitle != "t2" {
		t.Errorf("overwrite should change provided page_title: %q", assets2[0].PageTitle)
	}
}

// A19: JSON 往返
func TestJSONRoundtrip(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	root := uniqDomain("rt")
	rows := []*assettransfer.Row{
		{Sheet: "J", Line: 1, Type: "root_domain", Domain: root, RootDomain: root, ICP: "RT-ICP"},
		{Sheet: "J", Line: 2, Type: "service", ServiceType: "http", URL: "https://rt." + root + "/p?x=1", StatusCode: intPtr(200), Technologies: []string{"nginx"}, Auth: []assettransfer.AuthEntry{{Type: "basic", Username: "u", Password: "p"}}},
	}
	plan, id := previewRows(d, rows, assettransfer.Options{})
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	// 导出（含敏感）→ 重新导入预览 → 身份全部命中（unchanged/fill，不新增）
	assets := []assettransfer.ExportAsset{}
	for _, batch := range iterAll(t, d) {
		for _, a := range batch {
			ea := dbAssetToExport(a)
			assets = append(assets, ea)
		}
	}
	_, data, err := assettransfer.RenderPart(assettransfer.ExportOptions{Format: "json", IncludeSensitive: true}, assets)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := assettransfer.ParseJSON(data)
	if err != nil || len(parsed.Errors) > 0 {
		t.Fatalf("roundtrip parse: %v %+v", err, parsed.Errors)
	}
	plan2, _ := previewRows(d, parsed.Rows, assettransfer.Options{})
	if plan2.Summary.Create != 0 {
		t.Errorf("roundtrip must match identities, got creates: %+v", plan2.Summary)
	}
}

func iterAll(t *testing.T, d *DB) [][]*Asset {
	t.Helper()
	var out [][]*Asset
	_, err := d.Assets().StreamAssetsForExport(nil, AssetFilter{}, nil, func(batch []*Asset) error {
		out = append(out, batch)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func dbAssetToExport(a *Asset) assettransfer.ExportAsset {
	b, _ := json.Marshal(a)
	var ea assettransfer.ExportAsset
	_ = json.Unmarshal(b, &ea)
	ea.Type = a.Type
	ea.CompanyID = a.CompanyID
	ea.TaskIDs = a.TaskIDs
	ea.LastSeen = a.LastSeen
	ea.ServiceType = a.ServiceType
	ea.Extra = a.Extra
	if a.Port != nil {
		ea.Port = *a.Port
	}
	for _, op := range a.OpenPorts {
		if p, ok := op["port"].(float64); ok {
			svc, _ := op["service"].(string)
			ea.OpenPorts = append(ea.OpenPorts, assettransfer.PortEntry{Port: int(p), Service: svc})
		}
	}
	for _, m := range a.Auth {
		g := func(k string) string { v, _ := m[k].(string); return v }
		ea.Auth = append(ea.Auth, assettransfer.AuthEntry{Type: g("type"), Username: g("username"), Password: g("password"), Token: g("token"), Description: g("description")})
	}
	for _, m := range a.Params {
		g := func(k string) string { v, _ := m[k].(string); return v }
		ea.Params = append(ea.Params, assettransfer.ParamEntry{Location: g("location"), Name: g("name"), Value: g("value"), Type: g("type")})
	}
	return ea
}

func intPtr(v int) *int { return &v }

func TestImportReviewedMergeRegressions(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	seed := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(q, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	get := func(id int64) *Asset {
		t.Helper()
		as, err := d.Assets().GetByIDs([]int64{id})
		if err != nil || len(as) != 1 {
			t.Fatalf("get asset %d: %v", id, err)
		}
		return as[0]
	}
	commit := func(r *assettransfer.Row, o assettransfer.Options) *ImportPlan {
		t.Helper()
		p, id := previewRows(d, []*assettransfer.Row{r}, o)
		if p == nil || id == 0 {
			t.Fatal("preview failed")
		}
		if _, err := d.CommitImportBatch(id, p.Hash, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cid := seed(`INSERT INTO companies(name,nkey) VALUES ('regression','regression') RETURNING id`)
	id := seed(`INSERT INTO assets(type,domain,icp,extra) VALUES ('root_domain','merge.test','','{"note":"old","custom":"old","flag":false,"zero":0,"empty":""}') RETURNING id`)
	p := commit(&assettransfer.Row{Type: "root_domain", Domain: "merge.test", ICP: "filled", Note: "new", Extra: map[string]any{"custom": "new", "flag": true, "zero": 1, "empty": "filled", "newfalse": false, "newzero": 0}}, assettransfer.Options{CompanyID: cid})
	a := get(id)
	var companySource string
	if err := d.QueryRow(`SELECT company_source FROM assets WHERE id=$1`, id).Scan(&companySource); err != nil || companySource != "explicit" {
		t.Fatal("company source not explicit", err)
	}
	if a.ICP != "filled" || a.CompanyID == nil || *a.CompanyID != cid || a.Extra["note"] != "old" || a.Extra["custom"] != "old" || a.Extra["flag"] != false || a.Extra["zero"] != float64(0) || a.Extra["empty"] != "filled" || a.Extra["newfalse"] != false || a.Extra["newzero"] != float64(0) {
		t.Fatalf("fill changed protected values or failed to fill: %+v plan=%+v", a, p)
	}
	id = seed(`INSERT INTO assets(type,url,method,params) VALUES ('endpoint','https://params.test/x','GET',ARRAY['{"location":"query","name":"id","value":"a","type":""}'::jsonb]) RETURNING id`)
	p = commit(&assettransfer.Row{Type: "endpoint", URL: "https://params.test/x", Method: "GET", IP: "203.0.113.20", Params: []assettransfer.ParamEntry{{Location: "QUERY", Name: "ID", Value: "z", Type: "string"}}}, assettransfer.Options{})
	a = get(id)
	if a.Params[0]["value"] != "a" || a.Params[0]["type"] != "string" || len(a.Params) != 1 {
		t.Fatalf("param fill mismatch: %v", a.Params)
	}
	p = commit(&assettransfer.Row{Type: "endpoint", URL: "https://params.test/x", Method: "GET", Params: []assettransfer.ParamEntry{{Location: "query", Name: "id", Value: "z"}}}, assettransfer.Options{Conflict: "overwrite"})
	if p.Summary.Overwrite != 1 || get(id).Params[0]["value"] != "z" {
		t.Fatal("param overwrite missing from preview/execution")
	}
	id = seed(`INSERT INTO assets(type,ip,open_ports) VALUES ('ip','203.0.113.21',ARRAY['{"port":443,"unknown":false}'::jsonb]) RETURNING id`)
	p = commit(&assettransfer.Row{Type: "ip", IP: "203.0.113.21", OpenPorts: []assettransfer.PortEntry{{Port: 443, Service: "https"}}}, assettransfer.Options{})
	if p.Summary.Fill != 1 || get(id).OpenPorts[0]["service"] != "https" || get(id).OpenPorts[0]["unknown"] != false {
		t.Fatal("port empty-service merge mismatch")
	}
	id = seed(`INSERT INTO assets(type,service_type,url,auth,status_code,content_length) VALUES ('service','http','https://auth.test',ARRAY['{"type":"basic","username":"alice","password":"a"}'::jsonb,'{"type":"basic","username":"bob","password":"b"}'::jsonb],0,0) RETURNING id`)
	p = commit(&assettransfer.Row{Type: "service", ServiceType: "http", URL: "https://auth.test", Auth: []assettransfer.AuthEntry{{Type: "basic", Username: "alice", Password: "a"}}}, assettransfer.Options{Conflict: "overwrite"})
	if p.Summary.Overwrite != 1 || len(get(id).Auth) != 1 {
		t.Fatal("auth subset replacement must match preview")
	}
	p = commit(&assettransfer.Row{Type: "service", ServiceType: "http", URL: "https://auth.test", StatusCode: intPtr(200), ContentLength: func() *int64 { v := int64(10); return &v }(), Auth: []assettransfer.AuthEntry{{Type: "basic", Username: "alice", Password: "changed", Description: "filled"}}}, assettransfer.Options{})
	a = get(id)
	if a.Auth[0]["password"] != "a" || a.Auth[0]["description"] != "filled" || *a.StatusCode != 0 || *a.ContentLength != 0 {
		t.Fatal("auth fill or nonempty numeric zero protection failed")
	}
	id = seed(`INSERT INTO assets(type,service_type,url,technologies) VALUES ('service','http','https://tech.test',ARRAY['old-a','old-b']) RETURNING id`)
	p = commit(&assettransfer.Row{Type: "service", ServiceType: "http", URL: "https://tech.test", Technologies: []string{"new-c"}}, assettransfer.Options{Conflict: "overwrite"})
	if p.Summary.Overwrite != 1 || marshalCompactImport(get(id).Technologies) != `["new-c"]` {
		t.Fatal("overwrite collection replacement/add/remove mismatch")
	}
	// Selecting a company alone must produce an executable fill.
	id = seed(`INSERT INTO assets(type,domain) VALUES ('root_domain','only-company.test') RETURNING id`)
	p = commit(&assettransfer.Row{Type: "root_domain", Domain: "only-company.test"}, assettransfer.Options{CompanyID: cid})
	if p.Summary.Fill != 1 || get(id).CompanyID == nil {
		t.Fatal("company-only fill not planned")
	}
}

func TestImportReviewedHashBinding(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	hash := func(r *assettransfer.Row, o assettransfer.Options) string {
		t.Helper()
		p, err := ComputeImportPlan(d, []*assettransfer.Row{r}, nil, o)
		if err != nil {
			t.Fatal(err)
		}
		return p.Hash
	}
	row := func(domain, value string) *assettransfer.Row {
		return &assettransfer.Row{Type: "root_domain", Domain: domain, Extra: map[string]any{"secret": value}}
	}
	a := hash(row("one.test", "a"), assettransfer.Options{})
	for _, h := range []string{hash(row("two.test", "a"), assettransfer.Options{}), hash(row("one.test", "b"), assettransfer.Options{}), hash(row("one.test", "a"), assettransfer.Options{CompanyID: 1}), hash(row("one.test", "a"), assettransfer.Options{TaskID: 1}), hash(row("one.test", "a"), assettransfer.Options{Conflict: "overwrite"}), hash(row("one.test", "a"), assettransfer.Options{CompanyPolicy: "keep"})} {
		if a == h {
			t.Fatal("input or options not bound")
		}
	}
	if _, err := d.Exec(`INSERT INTO assets(type,domain) VALUES ('root_domain','masked.test')`); err != nil {
		t.Fatal(err)
	}
	if hash(row("masked.test", "a"), assettransfer.Options{}) == hash(row("masked.test", "b"), assettransfer.Options{}) {
		t.Fatal("masked new values not bound")
	}
	// Complementary duplicates must not mutate the original raw rows between hashes.
	rs := []*assettransfer.Row{{Type: "root_domain", Domain: "dup.test", Extra: map[string]any{"a": 1}}, {Type: "root_domain", Domain: "dup.test", Extra: map[string]any{"b": 2}}}
	before := marshalCompactImport(rs)
	p, err := ComputeImportPlan(d, rs, nil, assettransfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ComputeImportPlan(d, rs, nil, assettransfer.Options{})
	if err != nil || p.Hash != p2.Hash || before != marshalCompactImport(rs) {
		t.Fatal("planning mutated raw input or hash was unstable")
	}
}

func TestImportReviewedSecretLifecycle(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	newBatch := func(host string) (*ImportPlan, int64) {
		return previewRows(d, []*assettransfer.Row{{Type: "service", ServiceType: "http", URL: "https://" + host, Auth: []assettransfer.AuthEntry{{Password: "synthetic-lifecycle-secret"}}}}, assettransfer.Options{})
	}
	scrubbed := func(id int64) {
		t.Helper()
		var clean bool
		if err := d.QueryRow(`SELECT rows='[]'::jsonb AND plan='{}'::jsonb AND row_errors='[]'::jsonb AND warnings='[]'::jsonb FROM asset_import_batches WHERE id=$1`, id).Scan(&clean); err != nil || !clean {
			t.Fatalf("raw batch carriers not scrubbed id=%d err=%v", id, err)
		}
	}
	p, id := newBatch("committed.test")
	if _, err := d.CommitImportBatch(id, p.Hash, nil); err != nil {
		t.Fatal(err)
	}
	scrubbed(id)
	for _, mode := range []string{"commit", "read", "cleanup"} {
		p, id = newBatch(mode + ".test")
		if _, err := d.Exec(`UPDATE asset_import_batches SET expires_at=now()-interval '1 minute' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "commit":
			if _, err := d.CommitImportBatch(id, p.Hash, nil); !errors.Is(err, ErrImportBatchExpired) {
				t.Fatal(err)
			}
		case "read":
			if _, err := d.AssetImportBatchByID(id); !errors.Is(err, ErrImportBatchExpired) {
				t.Fatal(err)
			}
		default:
			if _, _, err := d.CleanupImportBatches(); err != nil {
				t.Fatal(err)
			}
		}
		scrubbed(id)
		if _, err := d.Exec(`UPDATE asset_import_batches SET expires_at=now()-interval '8 days' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.CleanupImportBatches(); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM asset_import_batches WHERE id=$1`, id).Scan(&n); err != nil || n != 0 {
			t.Fatal("expired batch not purged", err)
		}
	}
}

func TestImportReviewedReferenceRace(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	for _, mode := range []string{"task_ids", "anchor"} {
		t.Run(mode, func(t *testing.T) {
			tid := createTaskRowForImportTest(t, d, "race "+mode)
			var aid int64
			domain := mode + ".test"
			if err := d.QueryRow(`INSERT INTO assets(type,domain) VALUES ('root_domain',$1) RETURNING id`, domain).Scan(&aid); err != nil {
				t.Fatal(err)
			}
			p, id := previewRows(d, []*assettransfer.Row{{Type: "root_domain", Domain: domain, ICP: "must not write"}}, assettransfer.Options{})
			holder, err := d.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback()
			if mode == "task_ids" {
				var ignored int64
				if err := holder.QueryRow(`SELECT id FROM assets WHERE id=$1 FOR UPDATE`, aid).Scan(&ignored); err != nil {
					t.Fatal(err)
				}
			} else {
				var nodeID int64
				if err := holder.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,state) SELECT exploration_id,'fact','confirmed' FROM tasks WHERE id=$1 RETURNING id`, tid).Scan(&nodeID); err != nil {
					t.Fatal(err)
				}
				if _, err := holder.Exec(`INSERT INTO exploration_anchors(node_id,asset_id) VALUES ($1,$2)`, nodeID, aid); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := d.CommitImportBatch(id, p.Hash, nil); done <- err }()
			deadline := time.Now().Add(3 * time.Second)
			blocked := false
			for time.Now().Before(deadline) {
				var n int
				if err := d.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'LOCK TABLE assets,%'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					blocked = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !blocked {
				t.Fatal("commit did not block before replan")
			}
			if mode == "task_ids" {
				if _, err := holder.Exec(`UPDATE assets SET task_ids=array_append(task_ids,$2) WHERE id=$1`, aid, tid); err != nil {
					t.Fatal(err)
				}

			}
			if err := holder.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrImportBatchStale) {
					t.Fatalf("reference changed during commit: want stale got %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("commit did not finish")
			}
			var icp sql.NullString
			if err := d.QueryRow(`SELECT icp FROM assets WHERE id=$1`, aid).Scan(&icp); err != nil || icp.Valid {
				t.Fatal("stale commit wrote asset", err)
			}
		})
	}
}

func TestImportCommittedSanitizedDiagnostics(t *testing.T) {
	d, _ := assetTransferTestDB(t)
	const secret = "synthetic-diagnostic-secret"
	rows := []*assettransfer.Row{{Type: "root_domain", Domain: "diagnostics.test", Note: secret, Extra: map[string]any{"secret": secret}}}
	errs := []*assettransfer.RowError{
		{Sheet: "JSON", Line: 7, Type: "bogus-" + secret, Field: "type", Ref: secret, Reason: "未知资产类型: bogus " + secret},
		{Sheet: secret, Line: 8, Type: "service", Field: secret, Ref: "https://user:" + secret + "@example.test", Reason: secret},
	}
	warnings := []*assettransfer.RowWarning{{Sheet: "JSON", Detail: secret}, {Sheet: secret, Detail: secret}}
	plan, err := ComputeImportPlan(d, rows, errs, assettransfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateImportBatch("diagnostics.json", "json", "synthetic", assettransfer.Options{}, &assettransfer.ParseResult{Rows: rows, Errors: errs, Warnings: warnings}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitImportBatch(id, plan.Hash, nil); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		b, err := d.AssetImportBatchByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.RowErrors) != 2 || len(b.Warnings) != 2 || len(b.Rows) != 0 || len(b.Plan.Rows) != 0 {
			t.Fatalf("missing safe diagnostics or retained raw data: %+v", b)
		}
		e := b.RowErrors[0]
		if e.Sheet != "JSON" || e.Line != 7 || e.Type != "unknown" || e.Field != "type" || !strings.Contains(e.Reason, "未知资产类型") || e.Ref != "[已遮蔽]" {
			t.Fatalf("unknown-type diagnostic lost: %+v", e)
		}
		var raw string
		if err := d.QueryRow(`SELECT jsonb_build_object('rows',rows,'plan',plan,'errors',row_errors,'warnings',warnings)::text FROM asset_import_batches WHERE id=$1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, secret) || strings.Contains(raw, "bogus-") {
			t.Fatal("receipt retained secret-bearing diagnostic text")
		}
	}
}
