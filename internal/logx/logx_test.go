package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriterRotatesAndKeepsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "app.log")
	w, err := NewRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 每行 20 字节，写 20 行共 400 字节，必然触发多次轮转。
	line := strings.Repeat("x", 19) + "\n"
	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	for _, p := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("期望存在 %s: %v", p, err)
		}
		if info.Size() > 100 {
			t.Fatalf("%s 超过大小上限: %d", p, info.Size())
		}
	}
	// backups=2 时不应产生第 3 份备份。
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatalf("backups=2 时不应存在 %s.3", path)
	}
}

func TestRotatingWriterAppendsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	w, err := NewRotatingWriter(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	w2, err := NewRotatingWriter(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	w2.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("应追加写入而不是覆盖，got %q", string(data))
	}
}
