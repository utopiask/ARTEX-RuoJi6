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
	// agent 自由键（401 观察 / 硬编码凭证值 / NTLM 挑战目标）属于合同内字段，敏感导出必须放行。
	agentAuth := &db.Asset{ID: 8, Auth: []map[string]any{
		{"type": "auth_required", "note": "全部路径 401"},
		{"type": "hardcoded_token", "value": "synthetic"},
		{"type": "ntlm_challenge_target", "target": "AMERICAS"},
	}}
	if err := validateAssetExportMapping(agentAuth, true); err != nil {
		t.Fatal(err)
	}
	out := exportAssetFromDB(agentAuth)
	if len(out.Auth) != 3 || out.Auth[0].Note != "全部路径 401" || out.Auth[1].Value != "synthetic" || out.Auth[2].Target != "AMERICAS" {
		t.Fatalf("agent auth keys lost in export conversion: %+v", out.Auth)
	}
}
