package server

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestAssetExportMappingRejectsSilentDrops(t *testing.T) {
	legacy := &db.Asset{ID: 1, Auth: []map[string]any{{"type": "custom", "unsupported": "synthetic-secret"}}}
	if err := validateAssetExportMapping(legacy, true); err == nil {
		t.Fatal("unknown authentication field silently accepted")
	}
	if err := validateAssetExportMapping(legacy, false); err != nil {
		t.Fatal("explicitly omitted authentication should not block a normal export", err)
	}
	for _, asset := range []*db.Asset{
		{ID: 2, OpenPorts: []map[string]any{{"port": 443.5}}},
		{ID: 3, OpenPorts: []map[string]any{{"port": 443, "unknown": true}}},
		{ID: 4, Params: []map[string]any{{"name": "id", "required": true}}},
		{ID: 5, Params: []map[string]any{{"name": "id", "value": false}}},
		{ID: 6, Extra: map[string]any{"note": 12}},
	} {
		if err := validateAssetExportMapping(asset, true); err == nil {
			t.Fatalf("unsupported legacy data accepted for asset %d", asset.ID)
		}
	}
	valid := &db.Asset{ID: 7, OpenPorts: []map[string]any{{"port": float64(443), "service": "https"}}, Auth: []map[string]any{{"type": "basic", "username": "fixture", "password": "synthetic"}}, Params: []map[string]any{{"location": "query", "name": "id", "value": "0"}}}
	if err := validateAssetExportMapping(valid, true); err != nil {
		t.Fatal(err)
	}
	if got := exportAssetFromDB(valid).ID; got != valid.ID {
		t.Fatalf("source reference lost: %d", got)
	}
}
