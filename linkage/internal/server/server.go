// Package server 提供 webhook 入口（net/http 标准库实现，可替换为 gin）。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"kernel-hw-linkage/internal/buffer"
	"kernel-hw-linkage/internal/config"
	"kernel-hw-linkage/internal/dedup"
	"kernel-hw-linkage/internal/model"
)

// Server HTTP 入口。
type Server struct {
	cfg   *config.Config
	dedup dedup.Deduper
	agg   *buffer.Aggregator
}

// New 构造 Server。
func New(cfg *config.Config, d dedup.Deduper, agg *buffer.Aggregator) *Server {
	return &Server{cfg: cfg, dedup: d, agg: agg}
}

// Handler 返回 http.Handler（路由 + 中间件）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/webhook", s.handleWebhook)
	return s.logging(mux)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("I! %s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 鉴权：共享令牌
	if s.cfg.Server.Token != "" {
		got := r.Header.Get("X-Webhook-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Server.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	var ev model.WebhookEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ev.Recovered() {
		log.Printf("I! skip recovered event %s", ev.Ident())
		w.WriteHeader(http.StatusOK)
		return
	}
	ident := ev.Ident()
	typ := ev.Type()
	if ident == "" || typ == "" {
		log.Printf("W! missing ident/type in payload: %+v", ev)
		http.Error(w, "missing ident/type", http.StatusBadRequest)
		return
	}
	ts := ev.Time()
	if ts == 0 {
		ts = time.Now().Unix()
	}

	// 幂等去重（ident, type, 窗口）；防御式兜底避免 dedup_minutes 配置为 0 时除零 panic
	dm := s.cfg.Window.DedupMinutes
	if dm <= 0 {
		dm = 10
	}
	win := ts / int64(dm*60)
	if !s.dedup.Acquire(ident, typ, win) {
		log.Printf("I! dedup skip %s/%s", ident, typ)
		w.WriteHeader(http.StatusOK)
		return
	}

	// 进入多类型合并缓冲（按去重窗口聚合）
	s.agg.Add(buffer.Event{
		Ident:       ident,
		Type:        typ,
		Window:      win,
		TriggerTime: ts,
		RuleName:    ev.Rule(),
	})
	log.Printf("I! enqueued %s/%s window=%d", ident, typ, win)
	w.WriteHeader(http.StatusOK)
}
