package sub

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// 样本一律使用合成凭据，格式与站点上的通用订阅保持一致。
// 真实订阅里的口令/UUID 属于第三方资产，不应进入仓库。

func ssSIP002Link(method, password, hostport, name string) string {
	cred := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
	return "ss://" + cred + "@" + hostport + "#" + url.PathEscape(name)
}

func vmessLinkOf(payload map[string]any) string {
	body, _ := json.Marshal(payload)
	return "vmess://" + base64.StdEncoding.EncodeToString(body)
}

func TestParseLinkSS(t *testing.T) {
	link := ssSIP002Link("rc4-md5", "testpass123", "203.0.113.10:8080", "🇸🇬 新加坡 01")
	p, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != "ss" || p.Server != "203.0.113.10" || p.Port != 8080 {
		t.Fatalf("节点字段不对: %+v", p)
	}
	if got := toString(p.Raw["cipher"]); got != "rc4-md5" {
		t.Errorf("cipher = %q", got)
	}
	if got := toString(p.Raw["password"]); got != "testpass123" {
		t.Errorf("password 未能正确解出")
	}
	if !strings.Contains(p.Name, "新加坡 01") {
		t.Errorf("节点名未解码: %q", p.Name)
	}
}

// 旧式订阅把 method:pass@host:port 整体做 base64，不带 @。
func TestParseLinkSSLegacy(t *testing.T) {
	inner := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:legacy-pass@198.51.100.7:8443"))
	p, err := ParseLink("ss://" + inner + "#old-style")
	if err != nil {
		t.Fatal(err)
	}
	if p.Server != "198.51.100.7" || p.Port != 8443 {
		t.Fatalf("节点字段不对: %+v", p)
	}
	if got := toString(p.Raw["cipher"]); got != "aes-256-gcm" {
		t.Errorf("cipher = %q", got)
	}
	if p.Name != "old-style" {
		t.Errorf("name = %q", p.Name)
	}
}

func TestParseLinkTrojan(t *testing.T) {
	link := "trojan://tr-pass-1@node.example.com:20028?security=tls&sni=sni.example.com&type=tcp#US-0260"
	p, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != "trojan" || p.Server != "node.example.com" || p.Port != 20028 {
		t.Fatalf("节点字段不对: %+v", p)
	}
	if got := toString(p.Raw["password"]); got != "tr-pass-1" {
		t.Errorf("password = %q", got)
	}
	if got := toString(p.Raw["sni"]); got != "sni.example.com" {
		t.Errorf("sni = %q", got)
	}
}

func TestParseLinkVMess(t *testing.T) {
	link := vmessLinkOf(map[string]any{
		"v": "2", "ps": "RELAY-01", "add": "104.18.42.163", "port": "443",
		"id": "11111111-2222-3333-4444-555555555555", "aid": "0",
		"net": "ws", "type": "none", "host": "cdn.example.com",
		"path": "/hls/test.m3u8", "tls": "tls", "sni": "cdn.example.com",
	})
	p, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != "vmess" || p.Server != "104.18.42.163" || p.Port != 443 {
		t.Fatalf("节点字段不对: %+v", p)
	}
	if got := toString(p.Raw["uuid"]); got != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("uuid = %q", got)
	}
	if got := toString(p.Raw["network"]); got != "ws" {
		t.Errorf("network = %q", got)
	}
	if v, _ := p.Raw["tls"].(bool); !v {
		t.Error("tls 应为 true")
	}
	if got := toString(p.Raw["servername"]); got != "cdn.example.com" {
		t.Errorf("servername = %q", got)
	}
	ws, _ := p.Raw["ws-opts"].(map[string]any)
	if got := toString(ws["path"]); got != "/hls/test.m3u8" {
		t.Errorf("ws path = %q", got)
	}
}

func TestParseLinkUnsupported(t *testing.T) {
	plugin := "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:p")) +
		"@1.1.1.1:443?plugin=obfs-local%3Bobfs%3Dhttp#x"
	for _, link := range []string{
		"hysteria2://pass@example.com:443?sni=example.com#HY2",
		// REALITY 需要伪造 ClientHello 指纹，gRPC / xhttp 传输也未实现。
		"vless://11111111-2222-3333-4444-555555555555@host:443?security=reality&type=tcp#x",
		"vless://11111111-2222-3333-4444-555555555555@host:443?security=tls&type=grpc#x",
		"ssr://xxx",
		"not-a-link",
		plugin, // 插件节点的探测行为与明文 SS 不同，属于不支持的范围
	} {
		if _, err := ParseLink(link); err == nil {
			t.Errorf("%s 应被判定为不支持", link)
		}
	}
}

// 订阅有两种常见编码：base64 包裹与明文，两种都要能解析。
func TestParseAny(t *testing.T) {
	plain := strings.Join([]string{
		ssSIP002Link("rc4-md5", "testpass123", "203.0.113.10:8080", "SG-01"),
		"trojan://tr-pass-1@node.example.com:443?security=tls&sni=sni.example.com#JP-01",
		vmessLinkOf(map[string]any{"v": "2", "ps": "VM-01", "add": "198.51.100.9",
			"port": "80", "id": "11111111-2222-3333-4444-555555555555", "aid": "0",
			"net": "tcp", "type": "none", "host": "", "path": "", "tls": ""}),
		"hysteria2://pass@example.com:443#HY2", // 不支持，应被丢弃
	}, "\n") + "\n"

	encoded := base64.StdEncoding.EncodeToString([]byte(plain))

	cases := map[string][]byte{
		"明文":        []byte(plain),
		"base64":    []byte(encoded),
		"base64带换行": []byte(chunk(encoded, 76)),
	}
	for name, data := range cases {
		nodes, err := ParseAny(data)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", name, err)
		}
		if len(nodes) != 3 {
			t.Errorf("%s 节点数 = %d want 3", name, len(nodes))
		}
	}
}

func TestParseAnyClashYAML(t *testing.T) {
	yamlDoc := `proxies:
  - {name: a, type: ss, server: 1.1.1.1, port: 8388, cipher: aes-256-gcm, password: p}
`
	nodes, err := ParseAny([]byte(yamlDoc))
	if err != nil || len(nodes) != 1 {
		t.Fatalf("YAML 解析失败: %v, %d", err, len(nodes))
	}
	// base64 包裹的 YAML（部分订阅如此）。
	wrapped := base64.StdEncoding.EncodeToString([]byte(yamlDoc))
	nodes, err = ParseAny([]byte(wrapped))
	if err != nil || len(nodes) != 1 {
		t.Fatalf("base64 YAML 解析失败: %v, %d", err, len(nodes))
	}
}

// ParseLink 是 Link 的逆运算，往返后关键字段必须一致。
func TestParseLinkRoundTrip(t *testing.T) {
	originals := []Proxy{
		testProxy("ss", map[string]any{"name": "SS 香港 🇭🇰", "server": "ss.example.com", "port": 8443,
			"cipher": "aes-256-gcm", "password": "p@ss:/word"}),
		testProxy("trojan", map[string]any{"name": "JP-01 | 东京", "server": "jp.example.com", "port": 443,
			"password": "a:b@c/d", "network": "ws", "sni": "cdn.example.com",
			"ws-opts": map[string]any{"path": "/ray?ed=2048", "headers": map[string]any{"Host": "ws.example.com"}}}),
		testProxy("vmess", map[string]any{"name": "VM-01", "server": "vm.example.com", "port": 443,
			"uuid": "11111111-2222-3333-4444-555555555555", "alterId": 0, "cipher": "auto",
			"tls": true, "servername": "sni.example.com", "network": "ws",
			"ws-opts": map[string]any{"path": "/vm", "headers": map[string]any{"host": "ws.example.com"}}}),
	}

	for _, orig := range originals {
		link, err := Link(orig)
		if err != nil {
			t.Fatalf("%s 渲染失败: %v", orig.Name, err)
		}
		back, err := ParseLink(link)
		if err != nil {
			t.Fatalf("%s 解析失败: %v (%s)", orig.Name, err, link)
		}
		if back.Name != orig.Name {
			t.Errorf("名称 = %q want %q", back.Name, orig.Name)
		}
		if back.Type != orig.Type || back.Server != orig.Server || back.Port != orig.Port {
			t.Errorf("%s 基础字段不一致: %+v", orig.Name, back)
		}
		for _, k := range []string{"uuid", "password", "cipher", "sni", "servername", "network"} {
			if toString(back.Raw[k]) != toString(orig.Raw[k]) {
				t.Errorf("%s 字段 %s 往返后不一致", orig.Name, k)
			}
		}
		if booleanish(back.Raw["tls"]) != booleanish(orig.Raw["tls"]) {
			t.Errorf("%s tls 不一致", orig.Name)
		}
		ow, _ := orig.Raw["ws-opts"].(map[string]any)
		bw, _ := back.Raw["ws-opts"].(map[string]any)
		if toString(ow["path"]) != toString(bw["path"]) {
			t.Errorf("%s ws path 往返后不一致", orig.Name)
		}
	}
}

func chunk(s string, n int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
		b.WriteString("\n")
	}
	return b.String()
}

// VLESS 链接有两种需要特别处理的写法：path 被编码两层（%252F），
// 以及 xtls-rprx-vision 的 flow 声明。前者解错会让 WS 全部 404。
func TestParseVLESSLink(t *testing.T) {
	link := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@vl.example.com:443" +
		"?type=ws&path=%252Fapi%252Fv1%252Fguests%252F5de1%252F&host=cdn.example.com" +
		"&security=tls&sni=cdn.example.com&flow=xtls-rprx-vision#VL-01"

	p, err := ParseLink(link)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Type != "vless" || p.Server != "vl.example.com" || p.Port != 443 {
		t.Fatalf("节点字段错误: %+v", p)
	}
	if got := toString(p.Raw["uuid"]); got != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Errorf("uuid = %q", got)
	}
	if !booleanish(p.Raw["tls"]) {
		t.Error("security=tls 应映射为 tls: true")
	}
	if got := toString(p.Raw["servername"]); got != "cdn.example.com" {
		t.Errorf("servername = %q", got)
	}
	ws, _ := p.Raw["ws-opts"].(map[string]any)
	if got := toString(ws["path"]); got != "/api/v1/guests/5de1/" {
		t.Errorf("两层编码的 path 应解到只剩一层，got %q", got)
	}
	if h, _ := ws["headers"].(map[string]any); toString(h["Host"]) != "cdn.example.com" {
		t.Errorf("ws Host = %v", ws["headers"])
	}
	if got := toString(p.Raw["flow"]); got != "xtls-rprx-vision" {
		t.Errorf("flow = %q", got)
	}
	if p.Name != "VL-01" {
		t.Errorf("name = %q", p.Name)
	}
}

// network 的几种别名都应按 tcp 处理，否则节点会被无谓地判为不支持。
func TestParseVLESSNetworkAliases(t *testing.T) {
	for _, netw := range []string{"", "tcp", "raw", "none"} {
		link := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@h.example.com:443?security=none"
		if netw != "" {
			link += "&type=" + netw
		}
		p, err := ParseLink(link)
		if err != nil {
			t.Fatalf("type=%q 应可解析: %v", netw, err)
		}
		if got := toString(p.Raw["network"]); got != "" {
			t.Errorf("type=%q 应归一化为 tcp，实际 %q", netw, got)
		}
	}
}
