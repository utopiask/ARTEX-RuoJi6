package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func transferAuthenticatedServer(t *testing.T) (*Server, *db.DB, *httptest.Server, string) {
	t.Helper()
	s, pg, _ := assetTransferServer(t)
	s.jwtKey = []byte("synthetic-asset-transfer-runtime-signing-key")
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s.Handler())
	t.Cleanup(host.Close)
	return s, pg, host, token
}

func transferHTTPRequest(t *testing.T, client *http.Client, base, token, method, path, contentType string, body []byte) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func transferHTTPPreview(t *testing.T, client *http.Client, base, token, content string) (int64, string) {
	t.Helper()
	ct, body := multipartPreview(t, "assets.json", content, "", `{"conflict":"fill"}`)
	status, data := transferHTTPRequest(t, client, base, token, "POST", "/api/assets/imports/preview", ct, body)
	if status != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", status, data)
	}
	var result struct {
		ID   int64  `json:"batch_id"`
		Hash string `json:"plan_hash"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.ID == 0 || result.Hash == "" {
		t.Fatalf("invalid preview: %v %s", err, data)
	}
	return result.ID, result.Hash
}

func TestAssetImportRealHTTPAuthAndReceiptReload(t *testing.T) {
	_, pg, host, token := transferAuthenticatedServer(t)
	client := host.Client()
	status, _ := transferHTTPRequest(t, client, host.URL, "", "GET", "/api/assets/import-template?format=csv", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated template=%d", status)
	}
	status, _ = transferHTTPRequest(t, client, host.URL, "", "POST", "/api/assets/imports/preview", "application/json", []byte(`{}`))
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated preview=%d", status)
	}
	status, _ = transferHTTPRequest(t, client, host.URL, token, "GET", "/api/assets/import-template?format=csv", "", nil)
	if status != http.StatusOK {
		t.Fatalf("authenticated template=%d", status)
	}
	id, hash := transferHTTPPreview(t, client, host.URL, token, `{"format":"artex.assets","schema_version":1,"assets":[{"type":"root_domain","domain":"http-auth.test"},{"type":"bogus","domain":"invalid.example.test"}]}`)
	var count int
	if err := pg.QueryRow(`SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preview wrote assets: %d, %v", count, err)
	}
	status, data := transferHTTPRequest(t, client, host.URL, token, "GET", fmt.Sprintf("/api/assets/imports/batch/%d/errors", id), "", nil)
	if status != http.StatusOK || !bytes.Contains(data, []byte("bogus")) {
		t.Fatalf("preview error download=%d %s", status, data)
	}
	payload, _ := json.Marshal(map[string]string{"plan_hash": hash})
	status, data = transferHTTPRequest(t, client, host.URL, token, "POST", fmt.Sprintf("/api/assets/imports/batch/%d/commit", id), "application/json", payload)
	if status != http.StatusOK {
		t.Fatalf("commit=%d %s", status, data)
	}
	var name string
	if err := pg.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	host.Close()
	pg.Close()
	admin, err := url.Parse(os.Getenv("ARTEX_ASSET_TRANSFER_TEST_DSN"))
	if err != nil || !strings.HasPrefix(name, "artex_test_asset_transfer_") {
		t.Fatal("invalid isolated database identity")
	}
	admin.Path = "/" + name
	reopened, err := db.Open(admin.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	batch, err := reopened.AssetImportBatchByID(id)
	if err != nil || batch.Status != "committed" || batch.Receipt == nil || len(batch.Receipt.CreatedIDs) != 1 {
		t.Fatalf("receipt after connection/runtime reload: %+v %v", batch, err)
	}
	if err := reopened.QueryRow(`SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("valid subset count=%d err=%v", count, err)
	}
}

func TestAssetImportHTTPTimeoutKeepsAtomicQueryableReceipt(t *testing.T) {
	_, pg, host, token := transferAuthenticatedServer(t)
	assetID, err := pg.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "timeout.test"})
	if err != nil {
		t.Fatal(err)
	}
	id, hash := transferHTTPPreview(t, host.Client(), host.URL, token, `{"format":"artex.assets","schema_version":1,"assets":[{"type":"root_domain","domain":"timeout.test","icp":"synthetic-icp"}]}`)
	blocker, err := pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`UPDATE assets SET domain=domain WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"plan_hash": hash})
	r, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/assets/imports/batch/%d/commit", host.URL, id), bytes.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 150 * time.Millisecond}
	response, err := client.Do(r)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("blocked write did not produce an HTTP timeout")
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		batch, err := pg.AssetImportBatchByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if batch.Status == "committed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed-out commit has no final receipt: %s", batch.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, data := transferHTTPRequest(t, host.Client(), host.URL, token, "POST", fmt.Sprintf("/api/assets/imports/batch/%d/commit", id), "application/json", payload)
	if status != http.StatusOK || !bytes.Contains(data, []byte(`"already_committed":true`)) {
		t.Fatalf("retry did not return original receipt: %d %s", status, data)
	}
}
