package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/assettransfer"
)

func TestAssetExportPartsFitByteLimitAndReimport(t *testing.T) {
	for _, format := range []string{"csv", "json"} {
		t.Run(format, func(t *testing.T) {
			rows := make([]assettransfer.ExportAsset, 400)
			for i := range rows {
				rows[i] = assettransfer.ExportAsset{Type: "root_domain", Domain: fmt.Sprintf("size-%d.test", i), Extra: map[string]any{"note": strings.Repeat("x", 60000)}}
			}
			parts, err := renderAssetExportParts(context.Background(), assettransfer.ExportOptions{Format: format, GeneratedAt: time.Unix(1, 0)}, rows)
			if err != nil {
				t.Fatal(err)
			}
			if len(parts) < 2 {
				t.Fatal("24 MB dataset was not split")
			}
			count := 0
			for _, part := range parts {
				if len(part.Data) > assettransfer.MaxUploadBytes {
					t.Fatalf("oversized part: %d", len(part.Data))
				}
				parsed, err := assettransfer.Parse("part."+part.Ext, part.Data, "")
				if err != nil || len(parsed.Errors) != 0 {
					t.Fatalf("part cannot be reimported: %v %+v", err, parsed)
				}
				if len(parsed.Rows) != part.Rows {
					t.Fatalf("rows=%d expected=%d", len(parsed.Rows), part.Rows)
				}
				for _, row := range parsed.Rows {
					if len(row.Note) != 60000 {
						t.Fatal("text truncated")
					}
				}
				count += len(parsed.Rows)
			}
			if count != 400 {
				t.Fatalf("lost rows: %d", count)
			}
		})
	}
}

func TestAssetExportUnsupportedXLSXFailsBeforeDownload(t *testing.T) {
	_, err := renderAssetExportParts(context.Background(), assettransfer.ExportOptions{Format: "xlsx"}, []assettransfer.ExportAsset{{Type: "root_domain", Domain: "long.example.test", Extra: map[string]any{"note": strings.Repeat("x", 40000)}}})
	if err == nil {
		t.Fatal("XLSX silently accepted an unrepresentable cell")
	}
}

func TestAssetExportPartsRespectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := renderAssetExportParts(ctx, assettransfer.ExportOptions{Format: "json"}, []assettransfer.ExportAsset{{Type: "root_domain", Domain: "cancel.example.test"}})
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}
