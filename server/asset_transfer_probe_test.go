package server

import (
	"errors"
	"testing"

	"github.com/Autumn-27/artex/assettransfer"
	"github.com/Autumn-27/artex/db"
)

func TestAssetImportProbeMatcher(t *testing.T) {
	for _, tc := range []struct {
		name, host, kind, domain, network string
		want                              bool
	}{
		{"root apex", "example.test", "root_domain", "example.test", "", true},
		{"root child", "api.example.test", "root_domain", "example.test", "", true},
		{"company child", "api.example.test", "domain", "example.test", "", true},
		{"suffix confusion", "evilexample.test", "domain", "example.test", "", false},
		{"suffix appended", "example.test.attacker.test", "root_domain", "example.test", "", false},
		{"exact subdomain", "api.example.test", "subdomain", "api.example.test", "", true},
		{"subdomain descendants denied", "child.api.example.test", "subdomain", "api.example.test", "", false},
		{"normalized domain", "API.Example.Test.", "domain", "EXAMPLE.TEST.", "", true},
		{"IPv4 inside", "192.0.2.19", "cidr", "", "192.0.2.0/24", true},
		{"IPv4 outside", "192.0.3.19", "cidr", "", "192.0.2.0/24", false},
		{"IPv6 inside", "2001:db8::42", "cidr", "", "2001:db8::/32", true},
		{"IPv6 outside", "2001:db9::42", "cidr", "", "2001:db8::/32", false},
		{"single IP cidr", "192.0.2.19", "ip", "", "192.0.2.19/32", true},
		{"single IP exact", "192.0.2.19", "ip", "", "192.0.2.19", true},
		{"single IP other", "192.0.2.20", "ip", "", "192.0.2.19", false},
		{"invalid CIDR", "192.0.2.19", "cidr", "", "192.0.2.0/99", false},
		{"no DNS resolution", "example.test", "cidr", "", "192.0.2.0/24", false},
		{"ICP not authorization", "example.test", "icp", "example.test", "", false},
		{"keyword not authorization", "example.test", "keyword", "example.test", "", false},
		{"company ID not a rule", "example.test", "company", "example.test", "", false},
		{"invalid host", "example.test/attacker", "domain", "example.test", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := assetImportProbeMatches(tc.host, []db.ScopeRule{{Kind: tc.kind, Domain: tc.domain, Net: tc.network}}); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestAssetImportProbeSupportedTargets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asset *db.Asset
		host  string
	}{
		{"DNS", &db.Asset{ID: 1, Type: "root_domain", Domain: "example.test"}, "example.test"},
		{"DNS ignores unrelated IP", &db.Asset{ID: 1, Type: "subdomain", Domain: "outside.test", IP: "192.0.2.1"}, "outside.test"},
		{"HTTP hostname", &db.Asset{ID: 1, Type: "service", ServiceType: "http", URL: "https://outside.test:8443/path?x=1", IP: "192.0.2.1", Domain: "example.test"}, "outside.test"},
		{"HTTP IPv6", &db.Asset{ID: 1, Type: "service", ServiceType: "http", URL: "http://[2001:db8::1]:8080/"}, "2001:db8::1"},
		{"unsupported IP", &db.Asset{ID: 1, Type: "ip", IP: "192.0.2.1"}, ""},
		{"unsupported endpoint", &db.Asset{ID: 1, Type: "endpoint", URL: "https://example.test"}, ""},
		{"unsupported app", &db.Asset{ID: 1, Type: "app", URL: "https://example.test"}, ""},
		{"non HTTP service", &db.Asset{ID: 1, Type: "service", ServiceType: "other", Domain: "example.test"}, ""},
		{"non HTTP scheme", &db.Asset{ID: 1, Type: "service", ServiceType: "http", URL: "file:///example.test"}, ""},
		{"invalid URL", &db.Asset{ID: 1, Type: "service", ServiceType: "http", URL: "https://%invalid"}, ""},
		{"missing ID", &db.Asset{Type: "root_domain", Domain: "example.test"}, ""},
		{"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, ok := assetImportProbeHost(tc.asset)
			if host != tc.host || ok != (tc.host != "") {
				t.Fatalf("got %q,%v want %q", host, ok, tc.host)
			}
		})
	}
}

func TestAssetImportProbeContextIntersection(t *testing.T) {
	companyID := int64(7)
	assets := []*db.Asset{
		{ID: 1, Type: "root_domain", Domain: "example.test"},
		{ID: 2, Type: "subdomain", Domain: "api.example.test"},
		{ID: 3, Type: "service", ServiceType: "http", URL: "https://192.0.2.20/x"},
		{ID: 4, Type: "service", ServiceType: "http", URL: "https://outside.test", IP: "192.0.2.20", CompanyID: &companyID, TaskIDs: []int64{9}},
		{ID: 5, Type: "ip", IP: "192.0.2.20"},
	}
	calls := 0
	load := importProbeScopeLoader{
		liveTask: func(id int64) (bool, error) { return id == 9, nil },
		taskScope: func(id int64) ([]db.TaskScope, error) {
			return []db.TaskScope{{Kind: "company", CompanyID: &companyID}, {Kind: "root_domain", Domain: "outside.test"}}, nil
		},
		companyScope: func(id int64) ([]db.ScopeRule, error) {
			calls++
			return []db.ScopeRule{{Kind: "domain", Domain: "example.test"}, {Kind: "cidr", Net: "192.0.2.0/24"}}, nil
		},
	}
	batch := &db.AssetImportBatch{Options: assettransfer.Options{TaskID: 9, CompanyID: 7}}
	allowed, skipped, err := filterAssetImportProbes(batch, assets, load)
	if err != nil || len(allowed) != 3 || skipped != 2 || calls != 1 {
		t.Fatalf("intersection/caching: allowed=%d skipped=%d calls=%d err=%v", len(allowed), skipped, calls, err)
	}
	for i, a := range allowed {
		if a.ID != int64(i+1) {
			t.Fatal("unexpected target or changed ordering")
		}
	}
	// Ownership, task membership and an unrelated resolved IP cannot satisfy the
	// selected company's rules for an HTTP URL hostname.
	batch.Options.TaskID = 0
	allowed, skipped, err = filterAssetImportProbes(batch, assets, load)
	if err != nil || len(allowed) != 3 || skipped != 2 {
		t.Fatal("company-only scope failed")
	}
	batch.Options = assettransfer.Options{}
	allowed, skipped, err = filterAssetImportProbes(batch, assets, importProbeScopeLoader{})
	if err != nil || len(allowed) != 0 || skipped != len(assets) {
		t.Fatal("missing context must skip everything without loaders")
	}
}

func TestAssetImportProbeScopeLoadFailClosed(t *testing.T) {
	sentinel := errors.New("scope unavailable")
	companyID := int64(7)
	assets := []*db.Asset{{ID: 1, Type: "root_domain", Domain: "example.test"}}
	batch := &db.AssetImportBatch{Options: assettransfer.Options{TaskID: 9, CompanyID: 7}}
	for _, stage := range []string{"live-error", "not-live", "task-error", "company-error"} {
		t.Run(stage, func(t *testing.T) {
			taskCalls, companyCalls := 0, 0
			load := importProbeScopeLoader{
				liveTask: func(int64) (bool, error) {
					if stage == "live-error" {
						return false, sentinel
					}
					return stage != "not-live", nil
				},
				taskScope: func(int64) ([]db.TaskScope, error) {
					taskCalls++
					if stage == "task-error" {
						return nil, sentinel
					}
					return []db.TaskScope{{Kind: "company", CompanyID: &companyID}}, nil
				},
				companyScope: func(int64) ([]db.ScopeRule, error) { companyCalls++; return nil, sentinel },
			}
			allowed, skipped, err := filterAssetImportProbes(batch, assets, load)
			if err == nil || len(allowed) != 0 || skipped != 1 {
				t.Fatal("loader failure granted a probe")
			}
			if stage != "not-live" && !errors.Is(err, sentinel) {
				t.Fatal("loader error did not propagate")
			}
			if (stage == "live-error" || stage == "not-live") && (taskCalls != 0 || companyCalls != 0) {
				t.Fatal("scopes read before live task validation")
			}
		})
	}
	// A task with ICP/keyword-only scope cannot authorize a domain probe.
	batch.Options.CompanyID = 0
	load := importProbeScopeLoader{liveTask: func(int64) (bool, error) { return true, nil }, taskScope: func(int64) ([]db.TaskScope, error) {
		return []db.TaskScope{{Kind: "icp", Value: "example.test"}, {Kind: "keyword", Value: "example.test"}}, nil
	}}
	allowed, skipped, err := filterAssetImportProbes(batch, assets, load)
	if err != nil || len(allowed) != 0 || skipped != 1 {
		t.Fatal("non-host scope granted a probe")
	}
}
