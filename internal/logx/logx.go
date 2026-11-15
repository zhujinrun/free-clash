// Package logx 提供带大小轮转的文件日志写入器。
//
// 服务模式下进程会长期运行，且没有终端可看输出，日志必须落盘并限制体积，
// 否则一块 30m 间隔的常驻服务迟早把磁盘写满。
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter 是 io.Writer 实现：写入超过 MaxSize 后自动轮转，
// 旧的日志依次改名为 .1、.2 …，最多保留 Backups 份。
type RotatingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	backups int

	file *os.File
	size int64
}

// NewRotatingWriter 打开（或创建）日志文件。maxSize 单位为字节，
// backups 为轮转保留份数；backups<=0 时退化为不轮转、直接追加。
func NewRotatingWriter(path string, maxSize int64, backups int) (*RotatingWriter, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	w := &RotatingWriter{path: path, maxSize: maxSize, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.maxSize > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件并滚动历史文件。调用方需持有锁。
func (w *RotatingWriter) rotate() error {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	if w.backups <= 0 {
		return w.open()
	}
	// 由旧到新依次后移：path.(n-1) -> path.n，path -> path.1
	oldest := fmt.Sprintf("%s.%d", w.path, w.backups)
	os.Remove(oldest)
	for i := w.backups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		to := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(from); err == nil {
			os.Rename(from, to)
		}
	}
	if _, err := os.Stat(w.path); err == nil {
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return err
		}
	}
	return w.open()
}

// Close 关闭底层文件。
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
