// Package server 提供本地 HTTP 订阅服务，方便 Clash 直接把本工具的输出当作订阅地址。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Snapshot 是当前对外提供的结果快照。
type Snapshot struct {
	// Config 是 Clash 格式（YAML）。
	Config []byte
	// Subscription 是 base64 编码的分享链接列表，给 v2rayN / v2rayA 等客户端用。
	Subscription []byte
	// Links 是未编码的分享链接，一行一个。
	Links     []byte
	Updated   time.Time
	Total     int
	Available int
	TopNodes  []string
}

// Server 对外暴露 /best.yaml、/best.txt、/best.links、/status 与 /refresh。
type Server struct {
	addr    string
	mu      sync.RWMutex
	snap    Snapshot
	refresh func(ctx context.Context) error
}

// New 创建服务。refresh 供 /refresh 端点触发重新探测，可为 nil。
func New(addr string, refresh func(ctx context.Context) error) *Server {
	return &Server{addr: addr, refresh: refresh}
}

// Update 替换当前快照。
func (s *Server) Update(snap Snapshot) {
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

// handler 组装路由，单独抽出来便于测试。
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleConfig)
	mux.HandleFunc("/best.yaml", s.handleConfig)
	mux.HandleFunc("/best.txt", s.handleSubscription)
	mux.HandleFunc("/best.links", s.handleLinks)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/refresh", s.handleRefresh)
	return mux
}

// Run 阻塞运行 HTTP 服务，直到 ctx 结束。
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		close(done)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

func (s *Server) snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	if len(snap.Config) == 0 {
		http.Error(w, "尚未完成首次探测，请稍后刷新", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="best.yaml"`)
	w.Write(snap.Config)
}

// handleSubscription 输出 base64 订阅，v2rayN / v2rayA / Shadowrocket 可直接导入。
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	if len(snap.Subscription) == 0 {
		http.Error(w, "尚未完成首次探测，请稍后刷新", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="best.txt"`)
	// 提示客户端定期回源更新；不支持该字段的客户端会忽略。
	w.Header().Set("Profile-Update-Interval", "1")
	w.Write(snap.Subscription)
}

// handleLinks 输出未编码的分享链接，方便人工核对或只取其中一两个节点。
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	if len(snap.Links) == 0 {
		http.Error(w, "尚未完成首次探测，请稍后刷新", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="best.links"`)
	w.Write(snap.Links)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"updated":   snap.Updated,
		"total":     snap.Total,
		"available": snap.Available,
		"top_nodes": snap.TopNodes,
		"links":     len(snap.Links) > 0,
	})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if s.refresh == nil {
		http.Error(w, "refresh not configured", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if err := s.refresh(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok\n"))
}
