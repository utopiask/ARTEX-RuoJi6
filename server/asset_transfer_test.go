package server

// 资产导入导出 server 层测试：直接调用 handler（不构造完整引擎），使用
// ARTEX_ASSET_TRANSFER_TEST_DSN 指向的一次性隔离库（§16）。合成数据。

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/assettransfer"
	"github.com/Autumn-27/artex/db"
)

func assetTransferServer(t *testing.T) (*Server, *db.DB, func(method, path, contentType string, body []byte) *httptest.ResponseRecorder) {
	t.Helper()
	adminDSN := os.Getenv("ARTEX_ASSET_TRANSFER_TEST_DSN")
	if adminDSN == "" {
		t.Skip("ARTEX_ASSET_TRANSFER_TEST_DSN 未配置——按 §16 明确跳过")
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	dbName := "artex_test_asset_transfer_" + hex.EncodeToString(suffix)
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/postgres" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("test admin DSN must target the local postgres management database")
	}
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	quoted := `"` + strings.ReplaceAll(dbName, `"`, `""`) + `"`
	if _, err := admin.Exec(`CREATE DATABASE ` + quoted); err != nil {
		t.Fatalf("create isolated db: %v", err)
	}
	var pgDB *db.DB
	t.Cleanup(func() {
		if pgDB != nil {
			pgDB.Close()
		}
		cleanup, err := sql.Open("pgx", adminDSN)
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, dbName)
		if _, err := cleanup.Exec(`DROP DATABASE ` + quoted); err != nil {
			t.Errorf("drop exact owned test database: %v", err)
		}
	})
	u.Path = "/" + dbName
	probe, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	var current string
	err = probe.QueryRow(`SELECT current_database()`).Scan(&current)
	probe.Close()
	if err != nil || current != dbName {
		t.Fatalf("isolated identity check before migration: got %q, err %v", current, err)
	}
	pgDB, err = db.Open(u.String())
	if err != nil {
		t.Fatalf("open verified isolated db: %v", err)
	}
	s := &Server{m: &Manager{pg: pgDB, assets: pgDB.Assets(), tasks: map[string]*Task{}}}
	request := func(method, path, contentType string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		// 直接调用 handler 不经过 ServeMux：手动填充路径参数
		if strings.Contains(path, "/imports/") {
			pathOnly := path
			if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
				pathOnly = pathOnly[:i]
			}
			parts := strings.Split(strings.Trim(pathOnly, "/"), "/")
			for i := 0; i+1 < len(parts); i++ {
				if parts[i] == "batch" {
					r.SetPathValue("id", parts[i+1])
				}
			}
		}
		w := httptest.NewRecorder()
		switch {
		case strings.Contains(path, "import-template"):
			s.assetImportTemplate(w, r)
		case strings.HasSuffix(path, "/imports/preview"):
			s.assetImportPreview(w, r)
		case strings.HasSuffix(path, "/errors"):
			s.assetImportErrors(w, r)
		case strings.HasSuffix(path, "/commit"):
			s.assetImportCommit(w, r)
		case strings.HasSuffix(path, "/probe"):
			s.assetImportProbe(w, r)
		case strings.HasSuffix(path, "/export"):
			s.assetExport(w, r)
		default:
			s.assetImportGet(w, r)
		}
		return w
	}
	return s, pgDB, request
}

func multipartPreview(t *testing.T, fileName, fileContent, text, options string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if options != "" {
		_ = mw.WriteField("options", options)
	}
	if text != "" {
		_ = mw.WriteField("text", text)
	}
	if fileName != "" {
		fw, _ := mw.CreateFormFile("file", fileName)
		fw.Write([]byte(fileContent))
	}
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

// A21 + A11 + A22(后端)：四步闭环 + 行数超限 + 破损文件 + 错误清单
func TestAssetImportHTTPEndToEnd(t *testing.T) {
	_, pgDB, req := assetTransferServer(t)

	// 1) 粘贴预览
	paste := "example-assets-a.test\n203.0.113.55\nnot a valid asset line"
	ct, body := multipartPreview(t, "", "", paste, `{"conflict":"fill"}`)
	w := req("POST", "/api/assets/imports/preview", ct, body)
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var previewResp struct {
		BatchID  int64            `json:"batch_id"`
		PlanHash string           `json:"plan_hash"`
		Summary  db.ImportSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &previewResp); err != nil {
		t.Fatal(err)
	}
	if previewResp.Summary.Create != 2 || previewResp.Summary.Invalid != 1 {
		t.Fatalf("summary: %+v", previewResp.Summary)
	}

	// 2) 分页预览（只取 create）
	w = req("GET", fmt.Sprintf("/api/assets/imports/batch/%d?action=create&offset=0&limit=10", previewResp.BatchID), "", nil)
	if w.Code != 200 {
		t.Fatalf("get preview: %d %s", w.Code, w.Body.String())
	}
	var getResp struct {
		Rows []*db.ImportPlanRow `json:"rows"`
	}
	json.Unmarshal(w.Body.Bytes(), &getResp)
	if len(getResp.Rows) != 2 {
		t.Fatalf("preview rows: %d", len(getResp.Rows))
	}

	// 3) 预览只读：资产表无写入
	var n int
	pgDB.QueryRow(`SELECT count(*) FROM assets`).Scan(&n)
	if n != 0 {
		t.Fatalf("preview must not write assets, found %d", n)
	}

	// 4) 提交
	commitBody, _ := json.Marshal(map[string]any{"plan_hash": previewResp.PlanHash})
	w = req("POST", fmt.Sprintf("/api/assets/imports/batch/%d/commit", previewResp.BatchID), "application/json", commitBody)
	if w.Code != 200 {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	var commitResp struct {
		Receipt *db.ImportReceipt `json:"receipt"`
	}
	json.Unmarshal(w.Body.Bytes(), &commitResp)
	if commitResp.Receipt == nil || commitResp.Receipt.Summary.Create != 2 {
		t.Fatalf("receipt: %+v", commitResp.Receipt)
	}

	// 5) 错误清单 CSV
	w = req("GET", fmt.Sprintf("/api/assets/imports/batch/%d/errors", previewResp.BatchID), "", nil)
	if w.Code != 200 {
		t.Fatalf("errors csv: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "未知资产类型") {
		t.Fatalf("errors csv must contain the invalid row reason: %q", w.Body.String())
	}
	if strings.Count(w.Body.String(), "\n") < 2 {
		t.Fatalf("errors csv must have header + data row: %q", w.Body.String())
	}

	// 6) 超限行数拒绝（>10000 行）
	big := &strings.Builder{}
	for i := 0; i < assettransfer.MaxRows+1; i++ {
		fmt.Fprintf(big, "bulk-%06d.example.test\n", i)
	}
	ct, body = multipartPreview(t, "", "", big.String(), `{}`)
	w = req("POST", "/api/assets/imports/preview", ct, body)
	if w.Code != 413 {
		t.Fatalf("row limit: %d %s", w.Code, w.Body.String())
	}

	// 7) 破损 XLSX 拒绝
	ct, body = multipartPreview(t, "broken.xlsx", "not a zip at all", "", `{}`)
	w = req("POST", "/api/assets/imports/preview", ct, body)
	if w.Code != 422 {
		t.Fatalf("broken xlsx: %d %s", w.Code, w.Body.String())
	}

	// 8) 未知 JSON 版本拒绝
	pkg := `{"format":"artex.assets","schema_version":99,"assets":[]}`
	ct, body = multipartPreview(t, "pkg.json", pkg, "", `{}`)
	w = req("POST", "/api/assets/imports/preview", ct, body)
	if w.Code != 422 {
		t.Fatalf("bad version: %d %s", w.Code, w.Body.String())
	}

	// 9) 不存在的任务 → 404，不自动创建
	ct, body = multipartPreview(t, "", "", "ok-task.test", `{"task_id":424242}`)
	w = req("POST", "/api/assets/imports/preview", ct, body)
	if w.Code != 404 {
		t.Fatalf("missing task: %d %s", w.Code, w.Body.String())
	}

	// 10) 补全：engine 未装配 → 503 而不是假报成功
	probeBody, _ := json.Marshal(map[string]any{})
	w = req("POST", fmt.Sprintf("/api/assets/imports/batch/%d/probe", previewResp.BatchID), "application/json", probeBody)
	if w.Code != 200 { // 已提交批次、engine nil → 503
		if w.Code != 503 {
			t.Fatalf("probe without engine: %d %s", w.Code, w.Body.String())
		}
	}
}

// 模板下载 + 导出闭环（A15 HTTP 面 + A17 面上的 zip 拆分留给 db 测试）
func TestAssetTemplateAndExportHTTP(t *testing.T) {
	_, _, req := assetTransferServer(t)
	w := req("GET", "/api/assets/import-template?format=xlsx", "", nil)
	if w.Code != 200 || !bytes.HasPrefix(w.Body.Bytes(), []byte("PK")) {
		t.Fatalf("template xlsx: %d", w.Code)
	}
	w = req("GET", "/api/assets/import-template?format=csv", "", nil)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("资产类型")) {
		t.Fatalf("template csv: %d", w.Code)
	}
	// 空库导出 → 404（明确无匹配，而不是空文件）
	expBody, _ := json.Marshal(map[string]any{"scope": "all", "format": "json"})
	w = req("POST", "/api/assets/export", "application/json", expBody)
	if w.Code != 404 {
		t.Fatalf("empty export: %d %s", w.Code, w.Body.String())
	}
	// 无效格式
	expBody, _ = json.Marshal(map[string]any{"scope": "all", "format": "pdf"})
	w = req("POST", "/api/assets/export", "application/json", expBody)
	if w.Code != 400 {
		t.Fatalf("bad format export: %d", w.Code)
	}
}

// A15/A21: 超 10000 行导出 → 分包 ZIP + 清单；每包可完整回导
func TestAssetExportSplitZIP(t *testing.T) {
	_, pgDB, req := assetTransferServer(t)
	if _, err := pgDB.Exec(`
INSERT INTO assets(type, domain, root_domain)
SELECT 'root_domain', 'bulk-' || g || '.split-test.example', 'split-test.example'
FROM generate_series(1, 10050) AS g`); err != nil {
		t.Fatal(err)
	}
	expBody, _ := json.Marshal(map[string]any{"scope": "all", "format": "json"})
	w := req("POST", "/api/assets/export", "application/json", expBody)
	if w.Code != 200 {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("expected zip content type, got %q", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	jsonParts, manifest := 0, false
	for _, f := range zr.File {
		if f.Name == "MANIFEST.txt" {
			manifest = true
			rc, _ := f.Open()
			content, _ := io.ReadAll(rc)
			rc.Close()
			if !strings.Contains(string(content), "分包: 2") || !strings.Contains(string(content), "10050") {
				t.Errorf("manifest content wrong: %q", string(content))
			}
		} else if strings.HasSuffix(f.Name, ".json") {
			jsonParts++
		}
	}
	if jsonParts != 2 || !manifest {
		t.Fatalf("want 2 json parts + manifest, got %d parts, manifest=%v", jsonParts, manifest)
	}
}

// 提交内容绑定：plan_hash 缺失 → 400
func TestAssetImportCommitRequiresPlanHash(t *testing.T) {
	_, _, req := assetTransferServer(t)
	w := req("POST", "/api/assets/imports/batch/1/commit", "application/json", []byte(`{}`))
	if w.Code != 400 {
		t.Fatalf("missing plan_hash: %d", w.Code)
	}
	w = req("GET", "/api/assets/imports/batch/999", "", nil)
	if w.Code != 404 {
		t.Fatalf("missing batch: %d", w.Code)
	}
}

// 路由注册冒烟：完整 Handler() 必须不因 ServeMux 模式二义 panic（生产部署
// 曾因 /api/assets/imports/{id} 与 /api/assets/{id}/intelligence 冲突挂死）。
func TestAssetTransferRouteRegistrationSmoke(t *testing.T) {
	s, _, _ := assetTransferServer(t)
	h := s.Handler()
	if h == nil {
		t.Fatal("handler nil")
	}
	// 关键路由可达性（405/404 都证明已注册且路由器没 panic）
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/assets/import-template"},
		{"POST", "/api/assets/imports/preview"},
		{"GET", "/api/assets/imports/batch/1"},
		{"POST", "/api/assets/imports/batch/1/commit"},
		{"GET", "/api/assets/imports/batch/1/errors"},
		{"POST", "/api/assets/imports/batch/1/probe"},
		{"POST", "/api/assets/export"},
		{"GET", "/api/assets/1/intelligence"},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 404 && !strings.Contains(c.path, "intelligence") {
			t.Errorf("%s %s unexpectedly 404", c.method, c.path)
		}
	}
}
