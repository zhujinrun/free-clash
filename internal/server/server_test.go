package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(snap Snapshot) *Server {
	s := New("127.0.0.1:0", nil)
	s.Update(snap)
	return s
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestEndpointsBeforeFirstProbe(t *testing.T) {
	s := New("127.0.0.1:0", nil)
	for _, path := range []string{"/", "/best.yaml", "/best.txt", "/best.links"} {
		if rec := get(t, s, path); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s 状态码 = %d want 503", path, rec.Code)
		}
	}
}

func TestEndpointsServeSnapshot(t *testing.T) {
	s := newTestServer(Snapshot{
		Config:       []byte("proxies: []\n"),
		Subscription: []byte("c3M6Ly9leGFtcGxlCg=="),
		Links:        []byte("ss://example\n"),
		Updated:      time.Now(),
		Total:        10,
		Available:    2,
		TopNodes:     []string{"a", "b"},
	})

	cases := []struct {
		path        string
		contentType string
		body        string
	}{
		{"/best.yaml", "text/yaml", "proxies: []"},
		{"/", "text/yaml", "proxies: []"},
		{"/best.txt", "text/plain", "c3M6Ly9leGFtcGxlCg=="},
		{"/best.links", "text/plain", "ss://example"},
	}
	for _, c := range cases {
		rec := get(t, s, c.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码 = %d want 200", c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.contentType) {
			t.Errorf("%s Content-Type = %q want 前缀 %q", c.path, ct, c.contentType)
		}
		if body := strings.TrimSpace(rec.Body.String()); body != c.body {
			t.Errorf("%s 内容 = %q want %q", c.path, body, c.body)
		}
	}
	if h := get(t, s, "/best.txt").Header().Get("Profile-Update-Interval"); h == "" {
		t.Error("订阅响应缺少 Profile-Update-Interval")
	}
}

func TestRefreshRequiresPost(t *testing.T) {
	// 未配置 refresh：直接 501。
	if rec := get(t, New("127.0.0.1:0", nil), "/refresh"); rec.Code != http.StatusNotImplemented {
		t.Errorf("未配置 refresh 时状态码 = %d want 501", rec.Code)
	}

	called := false
	s := New("127.0.0.1:0", func(context.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/refresh", nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /refresh 状态码 = %d want 405", rec.Code)
	}
	if called {
		t.Error("GET 不应触发刷新")
	}

	req = httptest.NewRequest(http.MethodPost, "/refresh", nil)
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Errorf("POST /refresh 状态码 = %d，called = %v", rec.Code, called)
	}
}
