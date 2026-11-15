package sub

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"testing"
)

// 解析结果只保留核对用的关键字段。
type parsedNode struct {
	kind     string
	server   string
	port     int
	secret   string
	network  string
	wsHost   string
	wsPath   string
	tls      bool
	sni      string
	skipCert bool
	name     string
}

// parseLink 用标准库独立解析分享链接，避免直接复用生成侧的逻辑。
func parseLink(t *testing.T, link string) parsedNode {
	t.Helper()

	switch {
	case len(link) > 5 && link[:5] == "ss://":
		u, err := url.Parse(link)
		if err != nil {
			t.Fatalf("ss 链接解析失败: %v (%s)", err, link)
		}
		cred, err := base64.RawURLEncoding.DecodeString(u.User.Username())
		if err != nil {
			t.Fatalf("ss 凭证解码失败: %v", err)
		}
		host, port := splitHostPort(t, u.Host)
		return parsedNode{kind: "ss", server: host, port: port, secret: string(cred), name: u.Fragment}

	case len(link) > 9 && link[:9] == "trojan://":
		u, err := url.Parse(link)
		if err != nil {
			t.Fatalf("trojan 链接解析失败: %v (%s)", err, link)
		}
		host, port := splitHostPort(t, u.Host)
		q := u.Query()
		return parsedNode{
			kind:     "trojan",
			server:   host,
			port:     port,
			secret:   u.User.Username(),
			network:  q.Get("type"),
			wsHost:   q.Get("host"),
			wsPath:   q.Get("path"),
			tls:      q.Get("security") == "tls",
			sni:      q.Get("sni"),
			skipCert: q.Get("allowInsecure") == "1",
			name:     u.Fragment,
		}

	case len(link) > 8 && link[:8] == "vless://":
		u, err := url.Parse(link)
		if err != nil {
			t.Fatalf("vless 链接解析失败: %v (%s)", err, link)
		}
		host, port := splitHostPort(t, u.Host)
		q := u.Query()
		return parsedNode{
			kind:     "vless",
			server:   host,
			port:     port,
			secret:   u.User.Username(),
			network:  q.Get("type"),
			wsHost:   q.Get("host"),
			wsPath:   q.Get("path"),
			tls:      q.Get("security") == "tls",
			sni:      q.Get("sni"),
			skipCert: q.Get("allowInsecure") == "1",
			name:     u.Fragment,
		}

	case len(link) > 8 && link[:8] == "vmess://":
		body, err := base64.StdEncoding.DecodeString(link[8:])
		if err != nil {
			t.Fatalf("vmess 主体解码失败: %v", err)
		}
		var m map[string]string
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("vmess JSON 解析失败: %v", err)
		}
		port, _ := strconv.Atoi(m["port"])
		return parsedNode{
			kind:    "vmess",
			server:  m["add"],
			port:    port,
			secret:  m["id"],
			network: m["net"],
			wsHost:  m["host"],
			wsPath:  m["path"],
			tls:     m["tls"] == "tls",
			sni:     m["sni"],
			name:    m["ps"],
		}
	}
	t.Fatalf("无法识别的链接: %s", link)
	return parsedNode{}
}

func splitHostPort(t *testing.T, hp string) (string, int) {
	t.Helper()
	u, err := url.Parse("//" + hp)
	if err != nil {
		t.Fatalf("host 解析失败: %v (%s)", err, hp)
	}
	port, _ := strconv.Atoi(u.Port())
	return u.Hostname(), port
}

// TestLinkRoundTrip 把节点渲染成链接后再解析回来，关键字段必须保持一致。
// 这一步能抓到转义错误（口令含特殊字符、节点名含空格与 emoji）与字段错位。
func TestLinkRoundTrip(t *testing.T) {
	cases := []struct {
		proxy Proxy
		want  parsedNode
	}{
		{
			proxy: testProxy("ss", map[string]any{
				"name":     "SS 香港 01 🇭🇰",
				"server":   "ss.example.com",
				"port":     8443,
				"cipher":   "aes-256-gcm",
				"password": "p@ss:/word",
			}),
			want: parsedNode{kind: "ss", server: "ss.example.com", port: 8443,
				secret: "aes-256-gcm:p@ss:/word", name: "SS 香港 01 🇭🇰"},
		},
		{
			proxy: testProxy("trojan", map[string]any{
				"name":     "JP-01 | 东京",
				"server":   "jp.example.com",
				"port":     443,
				"password": "a:b@c/d",
				"network":  "ws",
				"sni":      "cdn.example.com",
				"ws-opts": map[string]any{
					"path":    "/ray?ed=2048",
					"headers": map[string]any{"Host": "ws.example.com"},
				},
			}),
			want: parsedNode{kind: "trojan", server: "jp.example.com", port: 443,
				secret: "a:b@c/d", network: "ws", wsHost: "ws.example.com",
				wsPath: "/ray?ed=2048", tls: true, sni: "cdn.example.com", name: "JP-01 | 东京"},
		},
		{
			proxy: testProxy("vmess", map[string]any{
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
			}),
			want: parsedNode{kind: "vmess", server: "vm.example.com", port: 443,
				secret: "b831381d-6324-4d53-ad4f-8cda48b30811", network: "ws",
				wsHost: "ws.example.com", wsPath: "/vm", tls: true,
				sni: "sni.example.com", name: "VM-01"},
		},
		{
			proxy: testProxy("vless", map[string]any{
				"name":       "VL-01 | 法兰克福",
				"server":     "vl.example.com",
				"port":       443,
				"uuid":       "b831381d-6324-4d53-ad4f-8cda48b30811",
				"tls":        true,
				"servername": "sni.example.com",
				"network":    "ws",
				"ws-opts": map[string]any{
					"path":    "/api/v1/guests/x?ed=2048",
					"headers": map[string]any{"Host": "ws.example.com"},
				},
			}),
			want: parsedNode{kind: "vless", server: "vl.example.com", port: 443,
				secret: "b831381d-6324-4d53-ad4f-8cda48b30811", network: "ws",
				wsHost: "ws.example.com", wsPath: "/api/v1/guests/x?ed=2048",
				tls: true, sni: "sni.example.com", name: "VL-01 | 法兰克福"},
		},
	}

	for _, c := range cases {
		link, err := Link(c.proxy)
		if err != nil {
			t.Fatalf("%s 渲染失败: %v", c.proxy.Name, err)
		}
		got := parseLink(t, link)
		if got != c.want {
			t.Errorf("%s 往返不一致\n got  %+v\n want %+v\n link %s", c.proxy.Name, got, c.want, link)
		}
	}
}
