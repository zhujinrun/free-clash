package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"freeclash/internal/sub"
)

// 订阅级缓存：把每条订阅的原始响应按地址落盘，某条源临时拉不动时（限流 403、
// 超时、EOF、返回半截），用上次的响应继续供节点。
//
// 早先只有「整池兜底」——所有源全挂了才读 last-sub.yaml。实际故障形态是
// 一部分源被限流、另一部分还好：50 条文章订阅里 20 条返回 403，整池兜底根本
// 不触发，节点池照样少两三成。按地址分别缓存能补上这个缺口。
const (
	// subCacheTTL 是缓存的有效期。免费订阅的节点每天换一轮，超过半天以上的
	// 快照里活节点已经寥寥，继续拿来探测只是浪费连接数。
	subCacheTTL = 12 * time.Hour
	// subCacheMaxFile 限制单条订阅落盘的大小，个别源会返回几十 MB 的合集。
	subCacheMaxFile = 8 << 20
	// subCacheMaxDir 限制整个缓存目录的总大小，超出时按修改时间淘汰最旧的。
	subCacheMaxDir = 192 << 20
)

func (r *runner) subCacheDir() string {
	return filepath.Join(r.cfg.OutputDir, "sub-cache")
}

// subCacheFile 用地址的摘要做文件名：地址里带 token 和查询参数，直接拿来当
// 文件名既有非法字符也会超长。
func (r *runner) subCacheFile(rawURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURL)))
	return filepath.Join(r.subCacheDir(), hex.EncodeToString(sum[:10])+".gz")
}

// saveSubCache 记录一条订阅这次拉到的原文，供后续失败时兜底。
//
// 落盘前先 gzip：这些订阅是同一批节点的重复描述，压缩率通常有 6~8 倍，
// 一轮十几 MB 的原文压到两三 MB，半小时一轮的写入量可以接受。
func (r *runner) saveSubCache(rawURL string, body []byte) {
	if r.cfg.SubCacheTTL.Std() <= 0 || len(body) == 0 || len(body) > subCacheMaxFile {
		return
	}
	if err := os.MkdirAll(r.subCacheDir(), 0o755); err != nil {
		r.log.Debug("创建订阅缓存目录失败", "err", err)
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return
	}
	if err := zw.Close(); err != nil {
		return
	}
	path := r.subCacheFile(rawURL)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		r.log.Debug("写订阅缓存失败", "err", err)
		return
	}
	// 先写临时文件再改名：服务随时可能被停掉，半截的 gzip 会在下次启动时
	// 解压失败，宁可保留上一份完整的。
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		r.log.Debug("替换订阅缓存失败", "err", err)
	}
}

// loadSubCache 读取某条订阅上次成功拉到的原文。过期或损坏时返回 false。
func (r *runner) loadSubCache(rawURL string) ([]byte, time.Duration, bool) {
	ttl := r.cfg.SubCacheTTL.Std()
	if ttl <= 0 {
		return nil, 0, false
	}
	path := r.subCacheFile(rawURL)
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, false
	}
	age := time.Since(st.ModTime())
	if age > ttl {
		os.Remove(path)
		return nil, 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		os.Remove(path)
		return nil, 0, false
	}
	defer zr.Close()
	body, err := io.ReadAll(io.LimitReader(zr, subCacheMaxFile))
	if err != nil || len(body) == 0 {
		os.Remove(path)
		return nil, 0, false
	}
	return body, age, true
}

// fetchSubNodes 拉取并解析一条订阅，失败时回退到该订阅上次的缓存。
// cached 表示这批节点来自缓存而不是本次拉取。
func (r *runner) fetchSubNodes(ctx context.Context, rawURL string, timeout time.Duration) (nodes []sub.Proxy, cached bool, err error) {
	if data, ferr := sub.Fetch(ctx, rawURL, timeout); ferr == nil {
		parsed, perr := sub.ParseAny(data)
		if perr == nil && len(parsed) > 0 {
			r.saveSubCache(rawURL, data)
			return parsed, false, nil
		}
		// 拉到了但解析不出节点（改版、空文件、登录页）：同样走兜底，
		// 但不要把空内容写进缓存，否则下次缓存也一起变空。
		if perr == nil {
			perr = errors.New("解析出 0 个节点")
		}
		err = perr
	} else {
		err = ferr
	}
	if ctx.Err() != nil {
		return nil, false, err
	}

	body, age, ok := r.loadSubCache(rawURL)
	if !ok {
		return nil, false, err
	}
	parsed, perr := sub.ParseAny(body)
	if perr != nil || len(parsed) == 0 {
		return nil, false, err
	}
	r.log.Warn("订阅拉不动，改用上次缓存的节点",
		"源", sub.SourceOf(rawURL),
		"节点数", len(parsed),
		"缓存年龄", age.Round(time.Minute).String(),
		"err", err)
	return parsed, true, nil
}

// pruneSubCache 清掉过期缓存，并在总量超限时按修改时间淘汰最旧的。
// 每轮调用一次即可，失败不影响主流程。
func (r *runner) pruneSubCache() {
	dir := r.subCacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	ttl := r.cfg.SubCacheTTL.Std()

	type item struct {
		path string
		mod  time.Time
		size int64
	}
	items := make([]item, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		it := item{path: filepath.Join(dir, e.Name()), mod: info.ModTime(), size: info.Size()}
		// 过期文件与上次没写完的临时文件一并清掉。
		if strings.HasSuffix(e.Name(), ".tmp") || (ttl > 0 && time.Since(it.mod) > ttl) {
			os.Remove(it.path)
			continue
		}
		items = append(items, it)
		total += it.size
	}
	if total <= subCacheMaxDir {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	removed := 0
	for _, it := range items {
		if total <= subCacheMaxDir {
			break
		}
		if err := os.Remove(it.path); err == nil {
			total -= it.size
			removed++
		}
	}
	if removed > 0 {
		r.log.Debug("订阅缓存超出上限，淘汰旧文件", "数量", removed, "剩余字节", total)
	}
}

// subCacheSummary 用于日志：本轮有多少条源走了缓存。
func subCacheSummary(cached, total int) string {
	if cached == 0 {
		return fmt.Sprintf("全部 %d 条实时拉取", total)
	}
	return fmt.Sprintf("%d/%d 条走缓存", cached, total)
}
