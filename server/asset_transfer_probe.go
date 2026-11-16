package server

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/db"
	"golang.org/x/net/idna"
)

// allowedAssetImportProbes restricts only the optional import enrichment step.
// Scope is read from existing rules; asset ownership and task membership do not
// authorize probing. The caller retains its existing enrichment/proxy route.
func (s *Server) allowedAssetImportProbes(batch *db.AssetImportBatch, assets []*db.Asset) ([]*db.Asset, int, error) {
	if batch == nil {
		return nil, len(assets), fmt.Errorf("asset import probe batch is missing")
	}
	if batch.Options.TaskID == 0 && batch.Options.CompanyID == 0 {
		return nil, len(assets), nil
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return nil, len(assets), fmt.Errorf("asset import probe scope store is unavailable")
	}
	return filterAssetImportProbes(batch, assets, importProbeScopeLoader{
		liveTask: func(taskID int64) (bool, error) {
			var live bool
			err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&live)
			return live, err
		},
		taskScope:    s.assetStore().ListTaskScope,
		companyScope: s.m.pg.Companies().GetScope,
	})
}

// Function dependencies keep scope loading fail-closed and testable without DB
// connections. Company rules are cached per invocation, never synthesized.
type importProbeScopeLoader struct {
	liveTask     func(int64) (bool, error)
	taskScope    func(int64) ([]db.TaskScope, error)
	companyScope func(int64) ([]db.ScopeRule, error)
}

func filterAssetImportProbes(batch *db.AssetImportBatch, assets []*db.Asset, load importProbeScopeLoader) ([]*db.Asset, int, error) {
	if batch == nil {
		return nil, len(assets), fmt.Errorf("asset import probe batch is missing")
	}
	taskID, companyID := batch.Options.TaskID, batch.Options.CompanyID
	if taskID < 0 || companyID < 0 {
		return nil, len(assets), fmt.Errorf("asset import probe context is invalid")
	}
	if taskID == 0 && companyID == 0 {
		return nil, len(assets), nil
	}
	cache := map[int64][]db.ScopeRule{}
	companyRules := func(id int64) ([]db.ScopeRule, error) {
		if rules, ok := cache[id]; ok {
			return rules, nil
		}
		if load.companyScope == nil {
			return nil, fmt.Errorf("company probe scope loader is unavailable")
		}
		rules, err := load.companyScope(id)
		if err != nil {
			return nil, fmt.Errorf("load company probe scope: %w", err)
		}
		cache[id] = rules
		return rules, nil
	}
	var taskRules, selectedCompanyRules []db.ScopeRule
	if taskID > 0 {
		if load.liveTask == nil || load.taskScope == nil {
			return nil, len(assets), fmt.Errorf("task probe scope loader is unavailable")
		}
		live, err := load.liveTask(taskID)
		if err != nil {
			return nil, len(assets), fmt.Errorf("validate probe task: %w", err)
		}
		if !live {
			return nil, len(assets), fmt.Errorf("selected probe task is missing, deleted or archived")
		}
		scopes, err := load.taskScope(taskID)
		if err != nil {
			return nil, len(assets), fmt.Errorf("load task probe scope: %w", err)
		}
		for _, scope := range scopes {
			switch scope.Kind {
			case "root_domain", "subdomain", "ip", "cidr":
				taskRules = append(taskRules, db.ScopeRule{Kind: scope.Kind, Domain: scope.Domain, Net: scope.Net})
			case "company":
				if scope.CompanyID == nil || *scope.CompanyID <= 0 {
					continue
				}
				rules, err := companyRules(*scope.CompanyID)
				if err != nil {
					return nil, len(assets), err
				}
				taskRules = append(taskRules, rules...)
			}
		}
	}
	if companyID > 0 {
		var err error
		selectedCompanyRules, err = companyRules(companyID)
		if err != nil {
			return nil, len(assets), err
		}
	}
	allowed := make([]*db.Asset, 0, len(assets))
	for _, asset := range assets {
		host, ok := assetImportProbeHost(asset)
		if !ok {
			continue
		}
		if taskID > 0 && !assetImportProbeMatches(host, taskRules) {
			continue
		}
		if companyID > 0 && !assetImportProbeMatches(host, selectedCompanyRules) {
			continue
		}
		allowed = append(allowed, asset)
	}
	return allowed, len(assets) - len(allowed), nil
}

// Only targets supported by the existing import queue are eligible. HTTP
// authorization uses the URL hostname exclusively, never the asset's IP column.
func assetImportProbeHost(asset *db.Asset) (string, bool) {
	if asset == nil || asset.ID <= 0 {
		return "", false
	}
	switch asset.Type {
	case "root_domain", "subdomain":
		host := normalizeAssetImportProbeHost(asset.Domain)
		return host, host != "" && net.ParseIP(host) == nil
	case "service":
		if asset.ServiceType != "http" {
			return "", false
		}
		u, err := url.Parse(asset.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
			return "", false
		}
		host := normalizeAssetImportProbeHost(u.Hostname())
		return host, host != ""
	default:
		return "", false
	}
}

func normalizeAssetImportProbeHost(raw string) string {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 {
		return ""
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return ""
			}
		}
	}
	return ascii
}

func assetImportProbeMatches(host string, rules []db.ScopeRule) bool {
	host = normalizeAssetImportProbeHost(host)
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	for _, rule := range rules {
		switch rule.Kind {
		case "domain", "root_domain", "subdomain":
			if ip != nil {
				continue
			}
			domain := normalizeAssetImportProbeHost(rule.Domain)
			if domain == "" || net.ParseIP(domain) != nil {
				continue
			}
			if host == domain || rule.Kind != "subdomain" && strings.HasSuffix(host, "."+domain) {
				return true
			}
		case "ip", "cidr":
			if ip == nil {
				continue
			}
			if _, network, err := net.ParseCIDR(rule.Net); err == nil {
				if network.Contains(ip) {
					return true
				}
			} else if rule.Kind == "ip" {
				if allowedIP := net.ParseIP(rule.Net); allowedIP != nil && allowedIP.Equal(ip) {
					return true
				}
			}
		}
	}
	return false
}
