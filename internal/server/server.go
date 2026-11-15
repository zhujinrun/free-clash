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
	Config    []byte
	Updated   time.Time
	Total     int
	Available int
	TopNodes  []string
}

// Server 对外暴露 /best.yaml、/status 与 /refresh。
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

// Run 阻塞运行 HTTP 服务，直到 ctx 结束。
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleConfig)
	mux.HandleFunc("/best.yaml", s.handleConfig)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/refresh", s.handleRefresh)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
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

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()

	if len(snap.Config) == 0 {
		http.Error(w, "尚未完成首次探测，请稍后刷新", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="best.yaml"`)
	w.Write(snap.Config)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"updated":   snap.Updated,
		"total":     snap.Total,
		"available": snap.Available,
		"top_nodes": snap.TopNodes,
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
