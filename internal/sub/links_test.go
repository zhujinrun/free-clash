package sub

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func testProxy(typ string, raw map[string]any) Proxy {
	raw["type"] = typ
	return Proxy{
		Name:   toString(raw["name"]),
		Type:   typ,
		Server: toString(raw["server"]),
		Port:   toInt(raw["port"]),
		Raw:    raw,
	}
}

func TestLinkTrojanWS(t *testing.T) {
	p := testProxy("trojan", map[string]any{
		"name":             "🇯🇵 JP-01 东京",
		"server":           "jp.example.com",
		"port":             443,
		"password":         "p@ss word/1",
		"network":          "ws",
		"skip-cert-verify": true,
		"ws-opts": map[string]any{
			"path":    "/ray?ed=2048",
			"headers": map[string]any{"Host": "cdn.example.com"},
		},
	})

	link, err := Link(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("链接无法解析: %v (%s)", err, link)
	}
	if u.Scheme != "trojan" {
		t.Errorf("scheme = %q", u.Scheme)
	}
	if u.Host != "jp.example.com:443" {
		t.Errorf("host = %q", u.Host)
	}
	if pw := u.User.Username(); pw != "p@ss word/1" {
		t.Errorf("password = %q", pw)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"security":      "tls",
		"type":          "ws",
		"host":          "cdn.example.com",
		"path":          "/ray?ed=2048",
		"allowInsecure": "1",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("参数 %s = %q want %q", k, got, want)
		}
	}
	if u.Fragment != p.Name {
		t.Errorf("节点名 = %q want %q", u.Fragment, p.Name)
	}
}

func TestLinkSS(t *testing.T) {
	p := testProxy("ss", map[string]any{
		"name":     "SS-01",
		"server":   "2001:db8::1",
		"port":     8388,
		"cipher":   "chacha20-ietf-poly1305",
		"password": "secret",
	})

	link, err := Link(p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("链接无法解析: %v (%s)", err, link)
	}
	if u.Scheme != "ss" {
		t.Errorf("scheme = %q", u.Scheme)
	}
	// url.Parse 会把 base64 凭证当作 userinfo，主机为 IPv6 字面量。
	if u.Host != "[2001:db8::1]:8388" {
		t.Errorf("host = %q", u.Host)
	}
	cred, err := base64.RawURLEncoding.DecodeString(u.User.Username())
	if err != nil {
		t.Fatalf("凭证不是 base64url: %v (%q)", err, u.User.Username())
	}
	if string(cred) != "chacha20-ietf-poly1305:secret" {
		t.Errorf("凭证 = %q", cred)
	}
}

func TestLinkVMessWS(t *testing.T) {
	p := testProxy("vmess", map[string]any{
		"name":       "VM-01",
		"server":     "vm.example.com",
		"port":       443,
		"uuid":       "b831381d-6324-4d53-ad4f-8cda48b30811",
		"alterId":    0,
		"cipher":     "auto",
		"tls":        true,
		"servername": "sni.example.com",
		"network":    "ws",
		"ws-opts": map[string]any{
			"path":    "/vm",
			"headers": map[string]any{"host": "ws.example.com"},
		},
	})

	link, err := Link(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "vmess://") {
		t.Fatalf("前缀不对: %s", link)
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(link, "vmess://"))
	if err != nil {
		t.Fatalf("主体不是 base64: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("主体不是 JSON: %v", err)
	}
	want := map[string]string{
		"v":    "2",
		"ps":   "VM-01",
		"add":  "vm.example.com",
		"port": "443",
		"id":   "b831381d-6324-4d53-ad4f-8cda48b30811",
		"aid":  "0",
		"net":  "ws",
		"type": "none",
		"host": "ws.example.com",
		"path": "/vm",
		"tls":  "tls",
		"sni":  "sni.example.com",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("字段 %s = %q want %q", k, got[k], v)
		}
	}
	// auto 不应下发成 scy，交给客户端默认值。
	if _, ok := got["scy"]; ok {
		t.Errorf("scy 不应出现: %q", got["scy"])
	}
}

func TestRenderSubscription(t *testing.T) {
	nodes := []Proxy{
		testProxy("ss", map[string]any{"name": "a", "server": "1.1.1.1", "port": 1, "cipher": "aes-256-gcm", "password": "x"}),
		testProxy("trojan", map[string]any{"name": "b", "server": "2.2.2.2", "port": 2, "password": "y"}),
		testProxy("vmess", map[string]any{"name": "c", "server": "3.3.3.3", "port": 3, "uuid": "u"}),
	}

	encoded := RenderSubscription(nodes)
	body, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatalf("订阅不是 base64: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != len(nodes) {
		t.Fatalf("链接数 = %d want %d", len(lines), len(nodes))
	}
	for i, prefix := range []string{"ss://", "trojan://", "vmess://"} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("第 %d 行应为 %s，实际 %s", i+1, prefix, lines[i])
		}
	}

	// 不支持的协议应被跳过，而不是产出无效链接。
	if got := Links([]Proxy{testProxy("vless", map[string]any{"name": "d", "server": "4.4.4.4", "port": 4})}); len(got) != 0 {
		t.Errorf("vless 不应被渲染, got %v", got)
	}
}
