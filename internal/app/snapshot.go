package app

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"freeclash/internal/server"
)

// loadPreviousSnapshot 读取上次运行留下的产出，用于在首轮探测完成之前继续对外提供订阅。
//
// 服务重启（换二进制、改配置、机器重启）后内存快照是空的，/best.yaml 和
// /best.txt 会一路 503 到首轮跑完——单轮四五分钟，赶上网络差要七八分钟。
// 这期间客户端回源拿到 503，有的客户端会直接把订阅清空。磁盘上的上一份产出
// 通常只有半小时旧，先顶上比空着强。
func loadPreviousSnapshot(dir string) (server.Snapshot, bool) {
	cfg, err := os.ReadFile(filepath.Join(dir, "best.yaml"))
	if err != nil || len(cfg) == 0 {
		return server.Snapshot{}, false
	}
	snap := server.Snapshot{Config: cfg, Warm: true}
	snap.Subscription, _ = os.ReadFile(filepath.Join(dir, "best.txt"))
	snap.Links, _ = os.ReadFile(filepath.Join(dir, "best.links"))
	if st, serr := os.Stat(filepath.Join(dir, "best.yaml")); serr == nil {
		snap.Updated = st.ModTime()
	}
	if len(snap.Links) == 0 && len(snap.Subscription) == 0 {
		// 只有 YAML 也够用，但两者都缺说明产出不完整，不如等首轮。
		return server.Snapshot{}, false
	}

	// 节点数以分享链接为准：一行一个，空行不算。
	for _, line := range bytes.Split(snap.Links, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			snap.Available++
			if name := linkName(string(line)); name != "" && len(snap.TopNodes) < 30 {
				snap.TopNodes = append(snap.TopNodes, name)
			}
		}
	}
	return snap, true
}

// linkName 取分享链接里 # 之后的节点名，用于 /status 展示。
func linkName(link string) string {
	i := strings.LastIndex(link, "#")
	if i < 0 || i+1 >= len(link) {
		return ""
	}
	name := strings.TrimSpace(link[i+1:])
	// 分享链接里的节点名是百分号编码的，解回来便于阅读。
	if decoded, err := url.PathUnescape(name); err == nil {
		return decoded
	}
	return name
}
