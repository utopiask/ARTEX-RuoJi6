package assettransfer

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// 本包不能 import db（db/asset_import.go 反向依赖本包的行模型），因此这里
// 镜像 db/nkey.go 与 db/assets.go 的少量纯函数。行为必须与 db 侧保持一致，
// db 侧的单元测试覆盖其原始实现。

// RootDomainLocal mirrors db.RootDomain (eTLD+1; 内部/非 ICANN 域名原样返回自身)。
func RootDomainLocal(host string) (root string, isApex bool) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || net.ParseIP(h) != nil {
		return h, true
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil || etld1 == "" {
		return h, true
	}
	return etld1, h == etld1
}

// calcCSegmentLocal mirrors db.calcCSegment (IPv4 /24, IPv6 /48)。
func calcCSegmentLocal(ipStr string) string {
	if ipStr == "" {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		parts := strings.Split(ipStr, ".")
		if len(parts) == 4 {
			return parts[0] + "." + parts[1] + "." + parts[2] + ".0/24"
		}
		return ""
	}
	_, ipnet, err := net.ParseCIDR(ipStr + "/48")
	if err != nil {
		return ""
	}
	return ipnet.String()
}

// NormalizeURLLocal mirrors db.normalizeURL: 小写 scheme/host，裸根路径去尾斜杠，
// 其余路径与查询保持原样（不删路径、不重排查询，方案 §7.1）。
func NormalizeURLLocal(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	s := u.String()
	if strings.HasSuffix(s, "/") && u.Path == "/" && u.RawQuery == "" {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// decodeStrictJSON unmarshals with a depth cap (方案 §13: JSON 深度 32)。
func decodeStrictJSON(data string, maxDepth int, v any) error {
	if err := checkJSONDepth(data, maxDepth); err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("JSON 后有多余内容")
	}
	return nil
}

func checkJSONDepth(data string, maxDepth int) error {
	dec := json.NewDecoder(strings.NewReader(data))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				depth++
				if depth > maxDepth {
					return fmt.Errorf("JSON 嵌套深度超过 %d", maxDepth)
				}
			} else {
				depth--
			}
		}
	}
}

// checkJSONCellSizes applies the same logical cell budget as tabular formats.
func checkJSONCellSizes(m map[string]any) error {
	for k, v := range m {
		if k == "source_ref" {
			continue
		}
		var size int
		if text, ok := v.(string); ok {
			size = len(text)
		} else {
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("field %s cannot be encoded", k)
			}
			size = len(b)
		}
		if size > MaxCellBytes {
			return fmt.Errorf("field %s exceeds %d byte cell limit", k, MaxCellBytes)
		}
	}
	return nil
}

func validateObjectFields(m map[string]any, fields map[string]bool) error {
	for k, v := range m {
		numeric, ok := fields[k]
		if !ok {
			return fmt.Errorf("unsupported field %s", k)
		}
		if !numeric {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("field %s must be a string", k)
			}
		}
	}
	return nil
}
