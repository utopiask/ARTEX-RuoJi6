package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/assettransfer"
)

type assetExportPart struct {
	Ext  string
	Data []byte
	Rows int
}

type assetExportRange struct{ start, end int }

// Partition conservatively before rendering, then check the actual files. Row
// limits alone cannot make a package reimportable when cells contain large text.
func renderAssetExportParts(ctx context.Context, opts assettransfer.ExportOptions, assets []assettransfer.ExportAsset) ([]assetExportPart, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("没有可导出的资产")
	}
	var ranges []assetExportRange
	start, estimated := 0, 0
	for i, a := range assets {
		body, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("资产数据无法序列化")
		}
		extra, err := json.Marshal(a.Extra)
		if err != nil {
			return nil, fmt.Errorf("资产扩展数据无法序列化")
		}
		size := len(body) + len(extra) + 256
		if i > start && (i-start >= assettransfer.ExportRowsPerFile || estimated+size > assettransfer.MaxUploadBytes/4) {
			ranges = append(ranges, assetExportRange{start, i})
			start, estimated = i, 0
		}
		estimated += size
	}
	ranges = append(ranges, assetExportRange{start, len(assets)})
	for {
		parts := make([]assetExportPart, 0, len(ranges))
		next := make([]assetExportRange, 0, len(ranges))
		split := false
		for i, span := range ranges {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			opts.PartIndex, opts.PartCount = i+1, len(ranges)
			ext, data, err := assettransfer.RenderPart(opts, assets[span.start:span.end])
			if err != nil {
				return nil, err
			}
			fits, err := assetExportFileFits(ext, data)
			if err != nil {
				return nil, err
			}
			if !fits {
				if span.end-span.start == 1 {
					return nil, fmt.Errorf("单条资产超出可回导文件限制，请减少扩展内容或选择其它格式")
				}
				middle := span.start + (span.end-span.start)/2
				next = append(next, assetExportRange{span.start, middle}, assetExportRange{middle, span.end})
				split = true
				continue
			}
			parts = append(parts, assetExportPart{Ext: ext, Data: data, Rows: span.end - span.start})
			next = append(next, span)
		}
		if !split {
			return parts, nil
		}
		// Re-render with the final part numbers; envelope metadata must describe
		// the returned package, including when splitting changes digit counts.
		ranges = next
	}
}

func assetExportFileFits(ext string, data []byte) (bool, error) {
	if len(data) > assettransfer.MaxUploadBytes {
		return false, nil
	}
	if ext != "xlsx" {
		return true, nil
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false, fmt.Errorf("生成的工作簿格式无效")
	}
	if len(reader.File) > assettransfer.MaxXLSXEntries {
		return false, nil
	}
	var size uint64
	for _, file := range reader.File {
		if file.UncompressedSize64 > uint64(assettransfer.MaxXLSXZipBytes)-size {
			return false, nil
		}
		size += file.UncompressedSize64
	}
	return true, nil
}
