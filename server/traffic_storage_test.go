package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Autumn-27/artex/traffic"
)

func TestTrafficStorageAPIUnavailable(t *testing.T) {
	s := &Server{m: &Manager{}}
	w := httptest.NewRecorder()
	s.getTrafficStorage(w, httptest.NewRequest(http.MethodGet, "/api/traffic/storage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got traffic.StorageStats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Ready || !got.Stale || got.Error != "traffic storage unavailable" || got.TotalBytes != 0 {
		t.Fatalf("unexpected unavailable state: %+v", got)
	}
}

func TestTrafficStorageAPIRequiresAuth(t *testing.T) {
	s := &Server{m: &Manager{}, jwtKey: []byte("traffic-storage-test-signing-key-32")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/traffic/storage", s.getTrafficStorage)
	handler := s.requireAuth(mux)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/traffic/storage", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", w.Code)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/traffic/storage", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTrafficStorageAPIReady(t *testing.T) {
	tr, err := traffic.Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	s := &Server{m: &Manager{traffic: tr}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		w := httptest.NewRecorder()
		s.getTrafficStorage(w, httptest.NewRequest(http.MethodGet, "/api/traffic/storage", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var got traffic.StorageStats
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Error != "" {
			t.Fatalf("measurement failed: %+v", got)
		}
		if got.Ready && !got.Stale && !got.Refreshing {
			if got.IndexBytes <= 0 || got.ExternalBytes < 0 || got.TotalBytes != got.IndexBytes+got.ExternalBytes || got.UpdatedAt == "" {
				t.Fatalf("invalid storage totals: %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("measurement did not complete: %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
