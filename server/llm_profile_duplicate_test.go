package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
)

func TestLLMProfileDuplicateNames(t *testing.T) {
	for _, tc := range []struct {
		base  string
		taken []string
		want  string
	}{
		{"DS", nil, "DS-2"},
		{"GLM-1", nil, "GLM-2"},
		{"DS", []string{"DS-2", "DS-3", "DS-5"}, "DS-4"},
		{"GLM-2", []string{"GLM-3", "GLM-4"}, "GLM-5"},
		{"GPT4", nil, "GPT4-2"},
		{"GLM-5.3", nil, "GLM-5.3-2"},
		{"GLM-0", nil, "GLM-0-2"},
		{"GLM-01", nil, "GLM-2"},
		{"GLM-1.5", nil, "GLM-1.5-2"},
		{"GLM 1", nil, "GLM 1-2"},
		{" GLM-1 \t", nil, "GLM-2"},
		{"  DS \t", nil, "DS-2"},
		{" \t", nil, "LLM-2"},
		{"GLM-9223372036854775807", nil, "GLM-9223372036854775808"},
		{"GLM-999999999999999999999999999999", []string{"GLM-1000000000000000000000000000000"}, "GLM-1000000000000000000000000000001"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			taken := map[string]bool{}
			for _, name := range tc.taken {
				taken[name] = true
			}
			before := len(taken)
			if got := nextProfileName(tc.base, taken); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if len(taken) != before {
				t.Fatal("changed supplied names")
			}
		})
	}
}

// Never resolve db.DSN here: it can fall back to the running app's config.
// The caller must create a fresh database and supply its exact name separately.
func llmCopyTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, name := os.Getenv("ARTEX_PG_DSN"), os.Getenv("ARTEX_LLM_COPY_TEST_DB")
	if dsn == "" || name == "" {
		t.Skip("requires explicit disposable ARTEX_PG_DSN and ARTEX_LLM_COPY_TEST_DB")
	}
	if !regexp.MustCompile(`^artex_test_llm_copy_[a-f0-9]{16,}$`).MatchString(name) {
		t.Fatal("refusing non-disposable database name")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/"+name || u.Port() != "5437" {
		t.Fatal("DSN must target the exact disposable database on port 5437")
	}
	probe, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open disposable database failed")
	}
	defer probe.Close()
	var actual string
	if err := probe.QueryRow(`SELECT current_database()`).Scan(&actual); err != nil {
		t.Fatal("verify disposable database failed")
	}
	if actual != name {
		t.Fatal("refusing schema changes: database name mismatch")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal("initialize disposable schema failed:", err)
	}
	t.Cleanup(func() { pg.Close() })
	return pg
}

func llmCopyTestServer(t *testing.T, pg *db.DB) (*Server, http.Handler, string) {
	t.Helper()
	// No New/NewManager: those initialize runtime services. With no active keyed
	// profile, reapplyActiveProfile exits before constructing any provider.
	s := &Server{m: &Manager{pg: pg, dir: t.TempDir()}, jwtKey: []byte("synthetic-copy-test-signing-key-32-bytes"), llmHealth: llmpool.NewRegistry(nil, nil)}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Handler(), token
}

func llmCopyRequest(h http.Handler, token, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/llm/profiles/"+id+"/duplicate", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func llmCopySave(t *testing.T, pg *db.DB, p db.LLMProfile) int64 {
	t.Helper()
	id, err := pg.SaveProfile(&p)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func llmCopyBody(t *testing.T, w *httptest.ResponseRecorder) (int64, string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("copy status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["id"] == nil || body["name"] == nil {
		t.Fatalf("response must contain only id/name: %s", w.Body.String())
	}
	var id int64
	var name string
	if err := json.Unmarshal(body["id"], &id); err != nil || id <= 0 {
		t.Fatal("invalid response id")
	}
	if err := json.Unmarshal(body["name"], &name); err != nil {
		t.Fatal(err)
	}
	return id, name
}

func TestLLMProfileDuplicateDatabase(t *testing.T) {
	pg := llmCopyTestDB(t)
	var modelCalls atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()
	s, h, token := llmCopyTestServer(t, pg)
	defer func() {
		if n := modelCalls.Load(); n != 0 {
			t.Errorf("unexpected model requests: %d", n)
		}
	}()

	t.Run("AuthenticationAndIDs", func(t *testing.T) {
		for _, tok := range []string{"", "invalid"} {
			if w := llmCopyRequest(h, tok, "1"); w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d", w.Code)
			}
		}
		for _, id := range []string{"0", "-1", "abc", "1.5", "9223372036854775808"} {
			if w := llmCopyRequest(h, token, id); w.Code != http.StatusBadRequest {
				t.Fatalf("id=%s status=%d", id, w.Code)
			}
		}
		if w := llmCopyRequest(h, token, "9223372036854775807"); w.Code != http.StatusNotFound {
			t.Fatalf("missing status=%d", w.Code)
		}
	})

	t.Run("AllFieldsAndRuntimeState", func(t *testing.T) {
		p := db.LLMProfile{Name: "Full", Format: "openai", BaseURL: endpoint.URL, Proxy: "http://127.0.0.1:1", Model: "synthetic-model", APIKey: "synthetic-copy-key-not-real", RatePerSecond: 1.25, RatePerMinute: 17.5, ContextWindowK: 333, ThinkingType: "enabled", ReasoningEffort: "high", Priority: -7, PoolExclude: true, Streaming: false, MaxTokens: 1234, MaxTokensField: "max_completion_tokens", SessionHeaderKey: "X-Synthetic-Session", Retry: db.RetryOverride{Connect: db.RetryRule{Attempts: -1, IntervalMS: 123}, Empty: db.RetryRule{Attempts: 2, IntervalMS: 456}, Stream: db.RetryRule{Attempts: 3, IntervalMS: 789}}}
		id := llmCopySave(t, pg, p)
		p.ID = id
		if err := pg.EnsureLLMRecordsTable(); err != nil {
			t.Fatal(err)
		}
		if err := pg.EnsureLLMUsageTable(); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"llm_records", "llm_usage"} {
			if _, err := pg.Exec(`INSERT INTO `+table+`(profile_name,model,status) VALUES ($1,'synthetic-model','error')`, p.Name); err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(time.Hour)
		if err := pg.SaveLLMHealth(db.LLMHealth{ProfileID: id, Fails: 2, Trips: 4, OpenUntil: &deadline, LastError: "synthetic quota"}); err != nil {
			t.Fatal(err)
		}
		state := llmpool.State{Fails: 2, Trips: 4, OpenUntil: deadline, LastError: "synthetic quota"}
		s.llmHealth.Restore(id, state)
		limiter := s.requestLimiterForProfile(id)
		// These rows are inert: there is no Manager or engine running them.
		task, err := pg.CreateTaskWithOptions("synthetic copy task", "no execution", db.TaskCreateOptions{LLMProfileIDs: []int64{id}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.MarkTaskLLMProfileQuotaExhausted(task.ID, id, "synthetic quota"); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Exec(`UPDATE agents SET llm_profile_id=$1 WHERE key='mainagent'`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Exec(`INSERT INTO conversations(agent_key,title,llm_profile_id) VALUES ('mainagent','synthetic copy conversation',$1)`, id); err != nil {
			t.Fatal(err)
		}
		// Snapshot entire rows, including timestamps and state not in LLMProfile.
		snapshot := func() string {
			var value string
			err := pg.QueryRow(`SELECT json_build_array(
			 (SELECT row_to_json(p) FROM llm_profiles p WHERE id=$1),
			 (SELECT row_to_json(h) FROM llm_profile_health h WHERE profile_id=$1),
			 (SELECT row_to_json(q) FROM task_llm_profiles q WHERE task_id=$2 AND profile_id=$1),
			 (SELECT row_to_json(t) FROM tasks t WHERE id=$2),
			 (SELECT row_to_json(a) FROM agents a WHERE key='mainagent'),
			 (SELECT json_agg(c) FROM conversations c WHERE llm_profile_id=$1),
			 (SELECT json_agg(r) FROM llm_records r WHERE profile_name='Full'),
			 (SELECT json_agg(u) FROM llm_usage u WHERE profile_name='Full'))::text`, id, task.ID).Scan(&value)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		before, gen := snapshot(), s.provCacheGen
		w := llmCopyRequest(h, token, fmt.Sprint(id))
		cloneID, name := llmCopyBody(t, w)
		if name != "Full-2" || cloneID == id {
			t.Fatal("wrong clone identity")
		}
		if strings.Contains(w.Body.String(), p.APIKey) || strings.Contains(w.Body.String(), "api_key") {
			t.Fatal("key leaked")
		}
		clone, err := pg.ProfileByID(cloneID)
		if err != nil {
			t.Fatal(err)
		}
		want := p
		want.ID, want.Name = cloneID, name
		if !reflect.DeepEqual(clone, &want) {
			t.Fatal("clone configuration differs (private fields omitted)")
		}
		if after := snapshot(); after != before {
			t.Fatal("source configuration, health, quota or bindings changed")
		}
		if s.llmHealth.Get(id) != state || s.llmHealth.Get(cloneID) != (llmpool.State{}) {
			t.Fatal("health reset or inherited")
		}
		if s.requestLimiterForProfile(id) != limiter || s.requestLimiterForProfile(cloneID) == limiter {
			t.Fatal("source quota reset or clone shares limiter")
		}
		if s.provCacheGen != gen+1 {
			t.Fatal("provider caches not invalidated")
		}
		for _, query := range []string{
			`SELECT count(*) FROM llm_profile_health WHERE profile_id=$1`,
			`SELECT count(*) FROM task_llm_profiles WHERE profile_id=$1`,
			`SELECT count(*) FROM tasks WHERE llm_profile_id=$1 OR active_llm_profile_id=$1`,
			`SELECT count(*) FROM agents WHERE llm_profile_id=$1`,
			`SELECT count(*) FROM conversations WHERE llm_profile_id=$1`,
		} {
			var count int
			if err := pg.QueryRow(query, cloneID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("clone inherited runtime state or bindings")
			}
		}
		for _, table := range []string{"llm_records", "llm_usage"} {
			var count int
			if err := pg.QueryRow(`SELECT count(*) FROM `+table+` WHERE profile_name=$1`, name).Scan(&count); err != nil || count != 0 {
				t.Fatal("clone inherited history or metering")
			}
		}
		// Editing through the existing HTTP save path with a blank key retains it.
		body, err := json.Marshal(struct {
			db.LLMProfile
			APIKey string `json:"api_key"`
		}{LLMProfile: *clone})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		edit := httptest.NewRecorder()
		h.ServeHTTP(edit, r)
		if edit.Code != http.StatusOK {
			t.Fatalf("edit status=%d", edit.Code)
		}
		after, err := pg.ProfileByID(cloneID)
		if err != nil || after.APIKey != p.APIKey {
			t.Fatal("blank key edit lost cloned key")
		}
		if snapshot() != before {
			t.Fatal("editing clone changed original")
		}
	})

	t.Run("ZeroValuesAndActiveSource", func(t *testing.T) {
		id := llmCopySave(t, pg, db.LLMProfile{Name: "Zero", Format: "anthropic", Model: "synthetic-zero"})
		if err := pg.SetActiveProfile(id); err != nil {
			t.Fatal(err)
		}
		defer pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		before, err := pg.ProfileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		cloneID, _ := llmCopyBody(t, llmCopyRequest(h, token, fmt.Sprint(id)))
		clone, err := pg.ProfileByID(cloneID)
		if err != nil {
			t.Fatal(err)
		}
		want := *before
		want.ID, want.Name, want.IsDefault = cloneID, "Zero-2", false
		if !reflect.DeepEqual(clone, &want) {
			t.Fatal("zero configuration/default state not preserved")
		}
		after, err := pg.ProfileByID(id)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("active original changed")
		}
		if s.llmOn {
			t.Fatal("test unexpectedly initialized LLM runtime")
		}
	})

	t.Run("RepeatedAndConcurrent", func(t *testing.T) {
		id := llmCopySave(t, pg, db.LLMProfile{Name: "Concurrent", Format: "openai-responses", Model: "synthetic", APIKey: "synthetic-concurrent-key"})
		llmCopySave(t, pg, db.LLMProfile{Name: "Concurrent-2", Format: "jev", Model: "synthetic"})
		_, name := llmCopyBody(t, llmCopyRequest(h, token, fmt.Sprint(id)))
		if name != "Concurrent-3" {
			t.Fatal("did not skip existing name")
		}
		_, name = llmCopyBody(t, llmCopyRequest(h, token, fmt.Sprint(id)))
		if name != "Concurrent-4" {
			t.Fatal("repeated copy collided")
		}
		const callers = 12
		results := make(chan *httptest.ResponseRecorder, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; results <- llmCopyRequest(h, token, fmt.Sprint(id)) }()
		}
		close(start)
		wg.Wait()
		close(results)
		seen := map[string]bool{}
		successes := 0
		for w := range results {
			if w.Code == http.StatusConflict {
				continue
			} // bounded contention is an explicit API result
			_, name := llmCopyBody(t, w)
			if seen[name] {
				t.Fatal("concurrent callers received same name")
			}
			seen[name] = true
			successes++
		}
		if successes == 0 {
			t.Fatal("no concurrent copy succeeded")
		}
		var count int
		if err := pg.QueryRow(`SELECT count(*) FROM llm_profiles WHERE name LIKE 'Concurrent-%'`).Scan(&count); err != nil || count != successes+3 {
			t.Fatalf("unexpected insert count=%d successes=%d", count, successes)
		}
	})

	t.Run("StorageErrorsAndBoundedConflicts", func(t *testing.T) {
		id := llmCopySave(t, pg, db.LLMProfile{Name: "Failure", Format: "openai", Model: "synthetic", APIKey: "synthetic-secret-storage"})
		// Force errors containing private values: diagnostics must never reach JSON.
		for _, tc := range []struct {
			name, code, constraint string
			status, attempts       int
		}{
			{"NameConflict", "23505", "llm_profiles_name_key", 409, 5},
			{"OtherUnique", "23505", "unrelated_key", 500, 1},
			{"OtherStorage", "23514", "llm_profiles_name_key", 500, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := pg.Exec(`CREATE SEQUENCE llm_copy_attempts;
				CREATE FUNCTION llm_copy_fail_insert() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
				 PERFORM nextval('llm_copy_attempts');
				 RAISE EXCEPTION 'synthetic-secret-storage' USING ERRCODE='` + tc.code + `', CONSTRAINT='` + tc.constraint + `', TABLE='llm_profiles', DETAIL=NEW.api_key;
				END $$;
				CREATE TRIGGER llm_copy_fail BEFORE INSERT ON llm_profiles FOR EACH ROW EXECUTE FUNCTION llm_copy_fail_insert()`)
				if err != nil {
					t.Fatal(err)
				}
				defer pg.Exec(`DROP TRIGGER llm_copy_fail ON llm_profiles; DROP FUNCTION llm_copy_fail_insert(); DROP SEQUENCE llm_copy_attempts`)
				gen := s.provCacheGen
				w := llmCopyRequest(h, token, fmt.Sprint(id))
				if w.Code != tc.status {
					t.Fatalf("status=%d want=%d", w.Code, tc.status)
				}
				if strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Body.String(), "llm_profiles_name_key") {
					t.Fatal("private diagnostics leaked")
				}
				var attempts int
				if err := pg.QueryRow(`SELECT last_value FROM llm_copy_attempts`).Scan(&attempts); err != nil || attempts != tc.attempts {
					t.Fatalf("attempts=%d want=%d err=%v", attempts, tc.attempts, err)
				}
				if gen != s.provCacheGen {
					t.Fatal("failed insert invalidated caches")
				}
			})
		}
		t.Run("NameConflictThenSuccess", func(t *testing.T) {
			_, err := pg.Exec(`CREATE SEQUENCE llm_copy_attempts;
			CREATE FUNCTION llm_copy_fail_insert() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
			 IF nextval('llm_copy_attempts') <= 2 THEN
			  RAISE EXCEPTION 'synthetic collision' USING ERRCODE='23505', CONSTRAINT='llm_profiles_name_key', TABLE='llm_profiles';
			 END IF;
			 RETURN NEW;
			END $$;
			CREATE TRIGGER llm_copy_fail BEFORE INSERT ON llm_profiles FOR EACH ROW EXECUTE FUNCTION llm_copy_fail_insert()`)
			if err != nil {
				t.Fatal(err)
			}
			defer pg.Exec(`DROP TRIGGER llm_copy_fail ON llm_profiles; DROP FUNCTION llm_copy_fail_insert(); DROP SEQUENCE llm_copy_attempts`)
			_, name := llmCopyBody(t, llmCopyRequest(h, token, fmt.Sprint(id)))
			if name != "Failure-2" {
				t.Fatal("incorrect name after retry")
			}
			var attempts int
			if err := pg.QueryRow(`SELECT last_value FROM llm_copy_attempts`).Scan(&attempts); err != nil || attempts != 3 {
				t.Fatalf("attempts=%d, want 3", attempts)
			}
		})
		// ProfileByID uses api_key; ListProfiles uses api_key_hint, so these
		// independently exercise the source-read and name-list failure paths.
		for _, col := range []string{"api_key", "api_key_hint"} {
			t.Run("ReadFailure_"+col, func(t *testing.T) {
				if _, err := pg.Exec(`ALTER TABLE llm_profiles RENAME COLUMN ` + col + ` TO llm_copy_hidden`); err != nil {
					t.Fatal(err)
				}
				defer pg.Exec(`ALTER TABLE llm_profiles RENAME COLUMN llm_copy_hidden TO ` + col)
				w := llmCopyRequest(h, token, fmt.Sprint(id))
				if w.Code != 500 || strings.Contains(w.Body.String(), "llm_copy_hidden") || strings.Contains(w.Body.String(), "api_key") {
					t.Fatalf("unsafe storage response: status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	})
}
