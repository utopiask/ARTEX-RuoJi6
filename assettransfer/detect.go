package assettransfer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Parse detects the input format locally (不依赖 LLM 猜文件，方案 D02)：
// 1) 指定 filename 时按扩展名；2) 内容嗅探（PK 头→xlsx，{ 开头→json，
// 含表头/逗号/制表符→csv）；3) 粘贴文本走 TXT 规则（或 { 开头→json）。
func Parse(filename string, data []byte, pastedText string) (*ParseResult, error) {
	if len(data) == 0 && pastedText != "" {
		data = []byte(pastedText)
	}
	if len(data) > MaxUploadBytes {
		return nil, parseErr(413, "上传数据超过 %d MiB 上限", MaxUploadBytes>>20)
	}
	name := strings.ToLower(strings.TrimSpace(filename))
	switch path.Ext(name) {
	case ".xlsx":
		return ParseXLSX(data)
	case ".xls", ".xlsm":
		return nil, parseErr(422, "第一版不支持 %s 文件，请另存为 .xlsx", path.Ext(name))
	case ".csv":
		return ParseCSVFromText(string(data)), nil
	case ".txt":
		return ParseTXT(string(data)), nil
	case ".json":
		return ParseJSON(data)
	}
	// 无扩展名/未知扩展名 → 内容嗅探
	if len(data) > 1 && data[0] == 'P' && data[1] == 'K' {
		return ParseXLSX(data)
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n\xEF\xBB\xBF")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return ParseJSON(data)
	}
	if looksLikeCSV(trimmed) {
		return ParseCSVFromText(string(data)), nil
	}
	return ParseTXT(string(data)), nil
}

func looksLikeCSV(data []byte) bool {
	head := data
	if len(head) > 4<<10 {
		head = head[:4<<10]
	}
	text := string(head)
	firstLine := text
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		firstLine = text[:i]
	}
	if strings.Contains(firstLine, ",") || strings.Contains(firstLine, "\t") {
		// 表头含逗号且出现已知列名/类型列 → CSV
		lower := strings.ToLower(firstLine)
		for _, key := range []string{"type", "资产类型", "domain", "域名", "url", "ip"} {
			if strings.Contains(lower, key) {
				return true
			}
		}
	}
	return false
}

// DetectFormat is the pure format sniff used for reporting.
func DetectFormat(filename string, data []byte) string {
	switch strings.ToLower(path.Ext(strings.TrimSpace(filename))) {
	case ".xlsx", ".csv", ".txt", ".json":
		return strings.TrimPrefix(strings.ToLower(path.Ext(strings.TrimSpace(filename))), ".")
	}
	if len(data) > 1 && data[0] == 'P' && data[1] == 'K' {
		return "xlsx"
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n\xEF\xBB\xBF")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return "json"
	}
	if looksLikeCSV(trimmed) {
		return "csv"
	}
	return "txt"
}

var _ = fmt.Sprintf
var _ = json.Marshal
