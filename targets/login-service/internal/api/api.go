// Package api 是这个服务的对外表面：收登录、给后台看数、报自己的健康。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"login-service/internal/daily"
	"login-service/internal/logstore"
)

type Server struct {
	Store logstore.Store

	// CachePath 是每日汇总的落点。它和 Store（原始记录）是两个东西：
	// 原始记录只追加，汇总随时可以从头再算一遍。
	CachePath string

	// Version 写进 /healthz 和响应头，灰度期用来确认现在跑的是哪一版。
	Version string

	mu       sync.Mutex
	logins   int64
	requests int64
	recent   []time.Duration // 最近若干次请求耗时，用来报 p99
}

var dayFormat = "2006-01-02"

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/daily", s.handleDaily)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return s.instrument(mux)
}

// WithVersion 把版本号贴在每个响应上。灰度期两个实例同时在跑，
// 没有它就只能靠猜"我现在问的是哪一版"。
func WithVersion(v string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Login-Service-Version", v)
		next.ServeHTTP(w, r)
	})
}

// instrument 记录请求数与耗时。观测指标就从这儿出——
// 没有它，06 的"每级灰度后有已完成的观测记录"就只能靠嘴说。
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		d := time.Since(start)

		s.mu.Lock()
		s.requests++
		s.recent = append(s.recent, d)
		if len(s.recent) > 2000 {
			s.recent = s.recent[len(s.recent)-2000:]
		}
		s.mu.Unlock()
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
		http.Error(w, `要一个 {"user_id": "..."}`, http.StatusBadRequest)
		return
	}
	if err := s.Store.Append(body.UserID, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.logins++
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, "ok %s\n", s.Version)
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "logins_recorded_total %d\n", s.logins)
	fmt.Fprintf(w, "http_requests_total %d\n", s.requests)
	fmt.Fprintf(w, "http_request_seconds_p99 %.4f\n", p99(s.recent))
}

// p99 取最近这批请求的第 99 百分位。样本少的时候就直接给最大值——
// 宁可贵一点，也不要在灰度期报一个看着很稳、其实没意义的数。
func p99(ds []time.Duration) float64 {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	i := int(float64(len(sorted)-1) * 0.99)
	return sorted[i].Seconds()
}

// handleDaily 是后台要的那个数字。
//
// 读的是缓存，不是现算——页面要求打开就能看到数，而原始记录会一直长。
//
// 缓存不存在就明确报错，**不退回现算**：退回现算会让"数据没重建"这件事
// 看起来一切正常，而两个口径不同的数字会同时在外面跑。宁可报错。
func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().In(time.Local).Format(dayFormat)
	}
	if _, err := time.ParseInLocation(dayFormat, day, time.Local); err != nil {
		http.Error(w, fmt.Sprintf("日期要写成 YYYY-MM-DD，收到的是 %q", day), http.StatusBadRequest)
		return
	}

	c, err := daily.Load(s.CachePath)
	if err != nil {
		http.Error(w, "每日汇总不可读（"+err.Error()+"）：先跑 make reindex", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"day": day, "count": c.Days[day]})
}

const indexHTML = `<!doctype html>
<meta charset="utf-8">
<title>登录记录</title>
<h1>登录记录</h1>
<p id="count">读数据中…</p>
<script>
fetch('/api/daily').then(r => r.json()).then(d => {
  document.getElementById('count').textContent = d.day + '：' + d.count;
});
</script>
`

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}
