package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"login-service/internal/daily"
	"login-service/internal/logstore"
)

func server(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	return &Server{
		Store:     logstore.New(filepath.Join(dir, "logins.jsonl")),
		CachePath: filepath.Join(dir, "daily_cache.json"),
	}
}

// reindex 是 make reindex 在测试里的样子：按当前口径把汇总重算一遍。
func reindex(t *testing.T, s *Server) {
	t.Helper()
	rs, err := s.Store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if err := daily.Build(rs).Save(s.CachePath); err != nil {
		t.Fatal(err)
	}
}

func TestLoginIsRecorded(t *testing.T) {
	s := server(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(`{"user_id":"u1"}`))

	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应该 200，实际 %d", rec.Code)
	}

	rs, err := s.Store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].UserID != "u1" {
		t.Fatalf("登录没落进记录: %+v", rs)
	}
}

func TestLoginWithoutUserIDIsRejected(t *testing.T) {
	s := server(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("POST", "/login", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("没有 user_id 应该 400，实际 %d", rec.Code)
	}
}

// 这一天每个人只登录一次：去重与不去重在这里是同一个数，
// 所以这条断言不锁死任何一种口径。
func TestDailyCountsOneDay(t *testing.T) {
	s := server(t)
	at := time.Date(2026, 9, 10, 9, 0, 0, 0, time.Local)
	for _, u := range []string{"u1", "u2", "u3"} {
		if err := s.Store.Append(u, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.Append("u9", at.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	reindex(t, s)

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/daily?day=2026-09-10", nil))

	var got struct {
		Day   string `json:"day"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if got.Day != "2026-09-10" || got.Count != 3 {
		t.Errorf("10 号应该有 3 条、且只算 10 号: %+v", got)
	}
}

func TestDailyRejectsBadDate(t *testing.T) {
	rec := httptest.NewRecorder()
	server(t).Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/daily?day=昨天", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("日期格式不对应该 400，实际 %d", rec.Code)
	}
}

// 缓存读不到时明确报错，不退回现算。
//
// 退回现算的后果不是"数字不准"，而是"两个口径不同的数字同时在外面跑"：
// 页面按新代码现算，缓存还是老口径，两边都看着正常。
func TestDailyFailsClosedWhenCacheMissing(t *testing.T) {
	s := server(t)
	if err := s.Store.Append("u1", time.Date(2026, 9, 10, 9, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	// 故意不 reindex：原始记录有，汇总没有。

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/daily?day=2026-09-10", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("汇总不可读应该 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 观测指标得有真的数字，06 的观测记录才有东西可写。
func TestMetricsExposeSomethingObservable(t *testing.T) {
	s := server(t)
	s.Routes().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{"http_requests_total", "http_request_seconds_p99"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 少了 %q:\n%s", want, body)
		}
	}
}

func TestHealthzCarriesVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	s := server(t)
	s.Version = "v9"
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), "v9") {
		t.Errorf("灰度期要靠版本号确认现在跑的是哪一版，/healthz 得报出来: %q", rec.Body.String())
	}
}
