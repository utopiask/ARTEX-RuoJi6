package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestAssetImportMaintenanceScrubsWithoutAnotherUpload(t *testing.T) {
	s, pg, req := assetTransferServer(t)
	ct, body := multipartPreview(t, "cleanup.json", `{"format":"artex.assets","schema_version":1,"assets":[{"type":"service","url":"https://cleanup.test","auth":[{"type":"basic","password":"synthetic-cleanup-secret"}]}]}`, "", `{"conflict":"fill"}`)
	w := req("POST", "/api/assets/imports/preview", ct, body)
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		ID int64 `json:"batch_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview.ID == 0 {
		t.Fatal("invalid preview", err)
	}
	if _, err := pg.Exec(`UPDATE asset_import_batches SET expires_at=now()-interval '1 second' WHERE id=$1`, preview.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.runAssetImportCleanup(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var status, raw string
		if err := pg.QueryRow(`SELECT status, rows::text || plan::text FROM asset_import_batches WHERE id=$1`, preview.ID).Scan(&status, &raw); err != nil {
			t.Fatal(err)
		}
		if status == "expired" {
			if bytes.Contains([]byte(raw), []byte("synthetic-cleanup-secret")) {
				t.Fatal("expiry left raw preview data behind")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("maintenance did not expire batch, status=%s", status))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop on cancellation")
	}
}
