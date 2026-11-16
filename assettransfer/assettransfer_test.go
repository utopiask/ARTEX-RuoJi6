package assettransfer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseTXTClassification(t *testing.T) {
	text := strings.Join([]string{
		"example.com",                       // root_domain
		"www.example.com",                   // subdomain
		"203.0.113.10",                      // ip
		"2001:db8::1",                       // ip (ipv6)
		"https://api.example.test/v1/x?q=1", // http service, path kept
		"GET https://api.example.test/v1/x", // endpoint
		"192.168.0.0/24",                    // CIDR → error
		"ssh 10.0.0.1",                      // METHOD URL 无效 URL → endpoint error
		"intranet.corp.local",               // 内部域名 → root_domain（尽力分类）
		"no-dot-name",                       // 行错误
		"1.2.3.4:8080",                      // host:port → 行错误
	}, "\n")
	res := ParseTXT(text)
	if len(res.Errors) != 4 {
		t.Fatalf("want 4 errors (cidr, bad-endpoint, no-dot, host:port), got %d: %+v", len(res.Errors), res.Errors)
	}
	if len(res.Rows) != 7 {
		t.Fatalf("want 7 rows (incl. intranet root), got %d: %+v", len(res.Rows), res.Rows)
	}
	wantTypes := []string{"root_domain", "subdomain", "ip", "ip", "service", "endpoint", "subdomain"}
	for i, want := range wantTypes {
		if res.Rows[i].Type != want {
			t.Errorf("row %d: want %s got %s (%+v)", i, want, res.Rows[i].Type, res.Rows[i])
		}
	}
	// URL 路径与查询保留（A02）
	if res.Rows[4].URL != "https://api.example.test/v1/x?q=1" {
		t.Errorf("http service url lost path/query: %q", res.Rows[4].URL)
	}
	if res.Rows[5].Method != "GET" || res.Rows[5].URL != "https://api.example.test/v1/x" {
		t.Errorf("endpoint parse wrong: %+v", res.Rows[5])
	}
	// 子域名本地计算根域名，不查 DNS
	if res.Rows[1].RootDomain != "example.com" {
		t.Errorf("subdomain root_domain = %q", res.Rows[1].RootDomain)
	}
	// 内部域名沿用现有根域名尽力分类（publicsuffix 视 corp.local 为可注册域）
	if res.Rows[6].Type != "subdomain" || res.Rows[6].RootDomain != "corp.local" {
		t.Errorf("internal host follows existing best-effort classification: %+v", res.Rows[6])
	}
}

func TestParseCSVRoundtripWithFormulaEscape(t *testing.T) {
	// 导出 → 回读往返（A18）：中文、引号、换行、公式样式、前导单引号
	assets := []ExportAsset{
		{
			Type: "root_domain", Domain: "example.test", ICP: "京ICP备\"引号\"号",
			Extra: map[string]any{"note": "=SUM(A1)\n中文备注"},
		},
		{
			Type: "subdomain", Domain: "a.example.test", RecordType: "CNAME",
			RecordValue: []string{"b.example.test", "'quoted"},
			Extra:       map[string]any{"note": "+cmd"},
		},
	}
	opts := ExportOptions{Format: "csv", IncludeSensitive: false}
	_, data, err := RenderPart(opts, assets)
	if err != nil {
		t.Fatal(err)
	}
	res := ParseCSVFromText(string(data))
	if len(res.Errors) > 0 {
		t.Fatalf("roundtrip parse errors: %+v", res.Errors)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(res.Rows))
	}
	r0 := res.Rows[0]
	if r0.Domain != "example.test" || r0.ICP != "京ICP备\"引号\"号" {
		t.Errorf("row0 lost quoting: %+v", r0)
	}
	if r0.Note != "=SUM(A1)\n中文备注" {
		t.Errorf("formula-style note roundtrip failed: %q", r0.Note)
	}
	r1 := res.Rows[1]
	if len(r1.RecordValue) != 2 || r1.RecordValue[1] != "'quoted" {
		t.Errorf("leading-quote value roundtrip failed: %+v", r1.RecordValue)
	}
	if r1.Note != "+cmd" {
		t.Errorf("+ prefix roundtrip failed: %q", r1.Note)
	}
}

func TestParseCSVPlainNoMarkerDoesNotStrip(t *testing.T) {
	// 普通 CSV（无 _artex_schema 标记）不得擅自剥离前导引号（§6.3）
	csv := "type,domain,note\nroot_domain,'example.test,'kept'\n"
	res := ParseCSVFromText(csv)
	if len(res.Errors) != 0 || len(res.Rows) != 1 {
		t.Fatalf("unexpected: %+v", res.Errors)
	}
	if res.Rows[0].Domain != "'example.test" || res.Rows[0].Note != "'kept'" {
		t.Errorf("plain csv values must stay untouched: %+v", res.Rows[0])
	}
}

func TestParseJSONContract(t *testing.T) {
	pkg := `{
	  "format": "artex.assets", "schema_version": 1,
	  "assets": [
	    {"type": "service", "url": "https://api.example.test", "technologies": ["nginx"], "page_title": "演示", "source_ref": {"asset_id": 101}},
	    {"type": "endpoint", "url": "https://api.example.test/x", "method": "post", "params": [{"location": "query", "name": "id", "value": "9"}]},
	    {"type": "app", "app_name": "演示App", "unknown_field": 1}
	  ]
	}`
	res, err := ParseJSON([]byte(pkg))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || len(res.Errors) != 1 {
		t.Fatalf("want 2 rows + 1 error, got %d/%d: %+v", len(res.Rows), len(res.Errors), res.Errors)
	}
	if res.Rows[0].Type != "service" || res.Rows[0].ServiceType != "http" || res.Rows[0].PageTitle != "演示" {
		t.Errorf("service row: %+v", res.Rows[0])
	}
	if res.Rows[0].SourceRef == nil || res.Rows[0].SourceRef.AssetID != 101 {
		t.Errorf("source_ref not kept: %+v", res.Rows[0].SourceRef)
	}
	if res.Rows[1].Method != "POST" || len(res.Rows[1].Params) != 1 || res.Rows[1].Params[0].Value != "9" {
		t.Errorf("endpoint row: %+v", res.Rows[1])
	}
	if !strings.Contains(res.Errors[0].Reason, "unknown_field") {
		t.Errorf("unknown field must be rejected explicitly: %+v", res.Errors[0])
	}
}

func TestParseJSONRejectsUnknownVersion(t *testing.T) {
	if _, err := ParseJSON([]byte(`{"format": "artex.assets", "schema_version": 99, "assets": []}`)); err == nil {
		t.Fatal("unknown schema_version must be rejected")
	}
	if _, err := ParseJSON([]byte(`{"format": "other.format", "schema_version": 1, "assets": []}`)); err == nil {
		t.Fatal("unknown format must be rejected")
	}
}

func TestTemplateXLSXAndReparse(t *testing.T) {
	tpl, err := TemplateXLSX()
	if err != nil {
		t.Fatal(err)
	}
	res, err := ParseXLSX(tpl)
	if err != nil {
		t.Fatalf("template must parse: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("template should contain no data rows, got %d", len(res.Rows))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("template sheets should all be known: %+v", res.Warnings)
	}

	// 填两行再回读
	assets := []ExportAsset{
		{Type: "ip", IP: "203.0.113.7", BoundDomains: []string{"a.example.test"}, OpenPorts: []PortEntry{{Port: 443, Service: "https"}}},
		{Type: "service", ServiceType: "other", IP: "203.0.113.7", Port: 22, ServiceName: "SSH"},
		{Type: "endpoint", URL: "https://a.example.test/x", Method: "POST", Params: []ParamEntry{{Location: "query", Name: "id"}}},
		{Type: "app", AppName: "演示应用", BundleID: "com.example.demo", Category: "工具"},
	}
	opts := ExportOptions{Format: "xlsx"}
	_, data, err := RenderPart(opts, assets)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := ParseXLSX(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Errors) != 0 {
		t.Fatalf("export→import errors: %+v", res2.Errors)
	}
	if len(res2.Rows) != 4 {
		t.Fatalf("want 4 rows, got %d", len(res2.Rows))
	}
}

func TestRowLimitsAndValidation(t *testing.T) {
	// 端口非法 / IP 非法 / 记录类型非法
	res := ParseTXT("203.0.113.10\nnot an ip line")
	_ = res
	csvText := "type,ip,port,service_name,domain\nservice,999.1.1.1,22,ssh,\n"
	r := ParseCSVFromText(csvText)
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Reason, "IPv4/IPv6") {
		t.Errorf("invalid ip must be row error: %+v", r.Errors)
	}
}

func TestSafeExportPolicyAllFormats(t *testing.T) {
	for _, format := range []string{"csv", "xlsx", "json"} {
		t.Run(format, func(t *testing.T) {
			assets := []ExportAsset{
				{Type: "service", ServiceType: "http", URL: "https://example.test", Auth: []AuthEntry{{Password: "PASSWORD_SENTINEL"}}, Extra: map[string]any{"secret": "EXTRA_SENTINEL", "note": "kept"}},
				{Type: "endpoint", URL: "https://example.test/x", Method: "GET", Params: []ParamEntry{{Location: "query", Name: "id", Value: "PARAM_SENTINEL"}}},
			}
			for _, include := range []bool{false, true} {
				_, data, err := RenderPart(ExportOptions{Format: format, IncludeSensitive: include}, assets)
				if err != nil {
					t.Fatal(err)
				}
				result, err := Parse("export."+format, data, "")
				if err != nil || len(result.Errors) != 0 || len(result.Rows) != 2 {
					t.Fatalf("roundtrip: %v %+v", err, result)
				}
				if include {
					if len(result.Rows[0].Auth) != 1 || result.Rows[0].Auth[0].Password != "PASSWORD_SENTINEL" || result.Rows[1].Params[0].Value != "PARAM_SENTINEL" {
						t.Fatal("explicit sensitive values lost")
					}
				} else {
					if len(result.Rows[0].Auth) != 0 || result.Rows[1].Params[0].Value != "" {
						t.Fatal("default policy leaked secrets")
					}
					if strings.Contains(string(data), "EXTRA_SENTINEL") {
						t.Fatal("default extra leaked")
					}
				}
			}
			if assets[0].Auth[0].Password != "PASSWORD_SENTINEL" || assets[1].Params[0].Value != "PARAM_SENTINEL" || assets[0].Extra["secret"] != "EXTRA_SENTINEL" {
				t.Fatal("render mutated caller assets")
			}
			for _, raw := range []string{"https://user:URL_SECRET@example.test/", "https://example.test/?access_token=URL_SECRET", "https://example.test/?api_key=URL_SECRET"} {
				a := []ExportAsset{{Type: "service", ServiceType: "http", URL: raw}}
				_, data, err := RenderPart(ExportOptions{Format: format}, a)
				if err == nil || len(data) != 0 || strings.Contains(err.Error(), "URL_SECRET") {
					t.Fatalf("unsafe URL decision: %v", err)
				}
				_, data, err = RenderPart(ExportOptions{Format: format, IncludeSensitive: true}, a)
				if err != nil {
					t.Fatal(err)
				}
				result, err := Parse("export."+format, data, "")
				if err != nil || len(result.Errors) != 0 || result.Rows[0].URL != raw {
					t.Fatal("URL identity changed")
				}
			}
		})
	}
}

func TestExportCellPhysicalAndByteLimits(t *testing.T) {
	for _, tc := range []struct {
		format, text string
		reject       bool
	}{
		{"xlsx", strings.Repeat("x", MaxXLSXCellChars), false},
		{"xlsx", strings.Repeat("x", MaxXLSXCellChars+1), true},
		{"xlsx", strings.Repeat("界", 22000), true},
		{"csv", strings.Repeat("x", 40000), false},
		{"csv", "=" + strings.Repeat("x", MaxCellBytes-1), false},
		{"json", strings.Repeat("x", MaxCellBytes), false},
		{"json", strings.Repeat("x", 40000), false},
		{"csv", strings.Repeat("x", MaxCellBytes+1), true},
		{"json", strings.Repeat("x", MaxCellBytes+1), true},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.format, len(tc.text)), func(t *testing.T) {
			_, data, err := RenderPart(ExportOptions{Format: tc.format}, []ExportAsset{{Type: "root_domain", Domain: "example.test", Extra: map[string]any{"note": tc.text}}})
			if tc.reject {
				if err == nil || len(data) != 0 {
					t.Fatal("oversized cell not rejected before write")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := Parse("export."+tc.format, data, "")
			if err != nil || len(result.Errors) != 0 || len(result.Rows) != 1 || result.Rows[0].Note != tc.text {
				t.Fatal("text truncated during roundtrip")
			}
		})
	}
}

func TestTemplateContractAndLists(t *testing.T) {
	data, err := TemplateXLSX()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		data []byte
	}{{"template.xlsx", data}, {"template.csv", TemplateCSV()}} {
		result, err := Parse(item.name, item.data, "")
		if err != nil || len(result.Rows) != 0 || len(result.Errors) != 0 {
			t.Fatalf("template rejected: %v %+v", err, result)
		}
	}
	for _, format := range []string{"csv", "xlsx", "json"} {
		a := []ExportAsset{{Type: "service", ServiceType: "http", URL: "https://example.test", Technologies: []string{"name;version", "[literal]", "quoted\"value"}}, {Type: "app", AppName: "test", AppICP: "APP-ICP"}}
		_, data, err := RenderPart(ExportOptions{Format: format}, a)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Parse("export."+format, data, "")
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("parse %v %+v", err, result)
		}
		for _, r := range result.Rows {
			if r.Type == "app" && r.AppICP != "APP-ICP" {
				t.Fatal("app_icp ambiguous")
			}
			if r.Type == "service" && (len(r.Technologies) != 3 || r.Technologies[0] != "name;version") {
				t.Fatal("list member split")
			}
		}
	}
}

func TestStrictJSONNestedAndExactIntegers(t *testing.T) {
	for _, asset := range []string{
		`{"type":"service","url":"https://example.test","auth":[{"type":"basic","unknown":"secret"}]}`,
		`{"type":"service","url":"https://example.test","auth":[{"password":false}]}`,
		`{"type":"endpoint","url":"https://example.test","method":"GET","params":[{"name":"id","value":0}]}`,
		`{"type":"ip","ip":"203.0.113.1","open_ports":[{"port":443,"protocol":"udp"}]}`,
		`{"type":"ip","ip":"203.0.113.1","open_ports":[{"port":443,"service":false}]}`,
		`{"type":"service","url":"https://example.test","technologies":["nginx",false,0]}`,
		`{"type":"service","url":"https://example.test","status_code":200.9}`,
		`{"type":"service","url":"https://example.test","content_length":12.7}`,
		`{"type":"service","url":"https://example.test","source_ref":{"future":1}}`,
		`{"type":"service","url":"https://example.test","note":false}`,
		`{"type":"service","url":"https://example.test","auth":null}`,
	} {
		input := `{"format":"artex.assets","schema_version":1,"assets":[` + asset + `]}`
		result, err := ParseJSON([]byte(input))
		if err == nil && len(result.Errors) == 0 {
			t.Fatalf("lossy structure accepted: %s", asset)
		}
	}
	for _, input := range []string{`{"format":"artex.assets","schema_version":1,"future_assets":[]}`, `{"format":"artex.assets","schema_version":1,"assets":[] ,"source":{"future":1}}`} {
		if _, err := ParseJSON([]byte(input)); err == nil {
			t.Fatal("unknown envelope accepted")
		}
	}
	result, err := ParseJSON([]byte(`{"format":"artex.assets","schema_version":1,"assets":[{"type":"service","url":"https://example.test","content_length":9223372036854775807}]}`))
	if err != nil || len(result.Errors) != 0 || result.Rows[0].ContentLength == nil || *result.Rows[0].ContentLength != 9223372036854775807 {
		t.Fatalf("integer rounded: %v %+v", err, result)
	}
	// The same nested validation applies to JSON cells in CSV/XLSX.
	if _, err := parseAuthJSON("CSV", 2, `[{"password":false}]`); err == nil {
		t.Fatal("CSV nested wrong type accepted")
	}
}

func TestParserBoundaryRegressions(t *testing.T) {
	if _, err := Parse("unknown.bin", []byte("P"), ""); err != nil {
		t.Fatal(err)
	}
	input := "type,domain,note,_artex_schema,_artex_csv_text\nroot_domain,example.test,'original,artex.assets.csv/1,note\n"
	result := ParseCSVFromText(input)
	if len(result.Rows) != 1 || len(result.Errors) != 0 || result.Rows[0].Note != "'original" {
		t.Fatal("business metadata swallowed or changed row")
	}
	note := strings.Repeat("x", MaxCellBytes+1)
	raw, _ := json.Marshal(map[string]any{"format": FormatName, "schema_version": SchemaVersion, "assets": []any{map[string]any{"type": "root_domain", "domain": "example.test", "note": note}}})
	for _, tc := range []struct {
		name string
		data []byte
	}{{"input.json", raw}, {"input.csv", []byte("type,domain,note\nroot_domain,example.test," + note + "\n")}} {
		result, err := Parse(tc.name, tc.data, "")
		if err == nil && len(result.Errors) == 0 {
			t.Fatal("oversized input cell accepted")
		}
	}
}
