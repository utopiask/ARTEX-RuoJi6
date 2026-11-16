package assettransfer

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

// TemplateXLSX builds the fixed import template: 一张「填写说明」+ 按类型命名的
// 六张工作表（service 拆 HTTP服务/其他服务），表头含中文列名，说明行标注
// 必填/类型/格式与示例（方案 §6.2）。
func TemplateXLSX() ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	// 填写说明
	const instr = "填写说明"
	if err := f.SetSheetName("Sheet1", instr); err != nil {
		return nil, err
	}
	instructions := [][]string{
		{"ARTEX 资产导入模板", ""},
		{"格式版本", fmt.Sprintf("%s / schema %d", FormatName, SchemaVersion)},
		{"支持类型", "根域名 / IP / 子域名 / 应用 / HTTP服务 / 其他服务 / 端点"},
		{"列名", "按各表第一行中文列名填写（英文列名同样接受）；不要增删改列名"},
		{"列表字段", "多个值用分号分隔，也可填 JSON 数组"},
		{"JSON 字段", "开放端口/认证/参数 填 JSON 数组，见各表说明行"},
		{"留空", "空白表示「文件未提供」，导入不会清空已有值"},
		{"重复", "同类型同身份的多行会先合并；矛盾值会显式报冲突"},
		{"限制", fmt.Sprintf("单批最多 %d 行，文件最大 %d MiB", MaxRows, MaxUploadBytes>>20)},
		{"安全", "本模板不含公式/宏；认证列仅在确有必要时填写"},
	}
	for i, row := range instructions {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		if err := f.SetSheetRow(instr, cell, &row); err != nil {
			return nil, err
		}
	}
	_ = f.SetColWidth(instr, "A", "A", 14)
	_ = f.SetColWidth(instr, "B", "B", 72)

	for _, sh := range sheets {
		if _, err := f.NewSheet(sh.name); err != nil {
			return nil, err
		}
		headers := make([]string, 0, len(sh.cols))
		for _, c := range sh.cols {
			h := c.zh
			if c.req {
				h += "*"
			}
			headers = append(headers, h)
		}
		cell, _ := excelize.CoordinatesToCellName(1, 1)
		if err := f.SetSheetRow(sh.name, cell, &headers); err != nil {
			return nil, err
		}
		var descs []any
		for _, c := range sh.cols {
			d := columnDescription(c)
			descs = append(descs, d)
		}
		cell2, _ := excelize.CoordinatesToCellName(1, 2)
		if err := f.SetSheetRow(sh.name, cell2, &descs); err != nil {
			return nil, err
		}
		for i := range sh.cols {
			col, _ := excelize.ColumnNumberToName(i + 1)
			_ = f.SetColWidth(sh.name, col, col, 22)
		}
		// 表头样式：只设置文字样式，不添加公式
		style, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
		_ = f.SetCellStyle(sh.name, "A1", func() string {
			c, _ := excelize.CoordinatesToCellName(len(sh.cols), 1)
			return c
		}(), style)
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TemplateCSV builds the unified CSV template: BOM + 表头（含资产类型列与导出
// 元数据列）。不放示例数据行——错误行会被解析器拒绝；填写说明见 XLSX 模板。
func TemplateCSV() []byte {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	all := append([]colDef{csvTypeCol}, csvUnionColumns()...)
	headers := make([]string, 0, len(all)+2)
	for _, c := range all {
		h := c.zh
		if c.req {
			h += "*"
		}
		headers = append(headers, h)
	}
	headers = append(headers, "_artex_schema", "_artex_csv_text")
	buf.WriteString(csvLine(headers))
	return buf.Bytes()
}

func csvLine(cells []string) string {
	var out strings.Builder
	for i, c := range cells {
		if i > 0 {
			out.WriteByte(',')
		}
		if strings.ContainsAny(c, ",\"\n\r") {
			out.WriteByte('"')
			out.WriteString(strings.ReplaceAll(c, `"`, `""`))
			out.WriteByte('"')
		} else {
			out.WriteString(c)
		}
	}
	out.WriteByte('\n')
	return out.String()
}

func columnDescription(c colDef) string {
	if c.json {
		return c.desc + "（JSON）"
	}
	return c.desc
}
