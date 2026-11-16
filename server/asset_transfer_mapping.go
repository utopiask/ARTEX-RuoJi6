package server

import (
	"fmt"

	"github.com/Autumn-27/artex/db"
)

// The transfer contract is narrower than arbitrary legacy JSON. Refuse data we
// cannot represent instead of silently dropping unknown keys or coercing values.
func validateAssetExportMapping(a *db.Asset, includeSensitive bool) error {
	stringsOnly := func(rows []map[string]any, allowed map[string]bool, field string) error {
		for _, row := range rows {
			for key, value := range row {
				if !allowed[key] {
					return fmt.Errorf("资产 #%d 的 %s 含当前迁移合同不支持的字段，未导出", a.ID, field)
				}
				if value != nil {
					if _, ok := value.(string); !ok {
						return fmt.Errorf("资产 #%d 的 %s 含非文本值，未导出", a.ID, field)
					}
				}
			}
		}
		return nil
	}
	for _, row := range a.OpenPorts {
		for key, value := range row {
			if key != "port" && key != "service" {
				return fmt.Errorf("资产 #%d 的端口含未支持的扩展字段，未导出", a.ID)
			}
			if key == "service" && value != nil {
				if _, ok := value.(string); !ok {
					return fmt.Errorf("资产 #%d 的端口服务名不是文本，未导出", a.ID)
				}
			}
		}
		port, err := anyToIntExport(row["port"])
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("资产 #%d 含无效端口，未导出", a.ID)
		}
	}
	if includeSensitive {
		if err := stringsOnly(a.Auth, map[string]bool{"type": true, "username": true, "password": true, "token": true, "description": true, "note": true, "value": true, "target": true}, "auth"); err != nil {
			return err
		}
	}
	if err := stringsOnly(a.Params, map[string]bool{"location": true, "name": true, "value": true, "type": true}, "params"); err != nil {
		return err
	}
	if value, ok := a.Extra["note"]; ok && value != nil {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("资产 #%d 的备注不是文本，未导出", a.ID)
		}
	}
	return nil
}
