package sub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Links 把节点渲染成 v2rayN / v2rayA / Shadowrocket 等客户端通用的分享链接。
// 无法表达的协议（理论上不会出现在探测结果里）会被跳过。
func Links(nodes []Proxy) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		link, err := Link(n)
		if err != nil {
			continue
		}
		out = append(out, link)
	}
	return out
}

// RenderSubscription 输出 base64 订阅内容：每行一个分享链接再整体编码，
// 这是 v2ray 系客户端最通用的订阅格式（Clash 的 YAML 它们读不了）。
func RenderSubscription(nodes []Proxy) []byte {
	links := Links(nodes)
	body := strings.Join(links, "\n")
	if body != "" {
		body += "\n"
	}
	return []byte(base64.StdEncoding.EncodeToString([]byte(body)))
}

// PlainLinks 输出未编码的分享链接，一行一个，便于人工查看或粘贴单个节点。
func PlainLinks(nodes []Proxy) []byte {
	links := Links(nodes)
	if len(links) == 0 {
		return nil
	}
	return []byte(strings.Join(links, "\n") + "\n")
}

// Link 渲染单个节点。仅支持本程序实际探测过的三种协议。
func Link(p Proxy) (string, error) {
	switch p.Type {
	case "ss":
		return ssLink(p)
	case "trojan":
		return trojanLink(p)
	case "vmess":
		return vmessLink(p)
	default:
		return "", fmt.Errorf("unsupported proxy type: %s", p.Type)
	}
}

// hostPort 拼接 host:port，IPv6 需要加方括号。
func hostPort(p Proxy) string {
	return net.JoinHostPort(p.Server, strconv.Itoa(p.Port))
}

// ssLink 输出 SIP002 格式：ss://base64url(method:password)@host:port#name
func ssLink(p Proxy) (string, error) {
	method := strings.ToLower(toString(p.Raw["cipher"]))
	password := toString(p.Raw["password"])
	if method == "" || password == "" {
		return "", fmt.Errorf("ss node missing cipher/password")
	}
	cred := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
	// base64url 只含 unreserved 字符，放进 userinfo 不会被二次转义。
	u := &url.URL{
		Scheme:   "ss",
		User:     url.User(cred),
		Host:     hostPort(p),
		Fragment: p.Name,
	}
	if plugin := ssPlugin(p.Raw); plugin != "" {
		u.RawQuery = url.Values{"plugin": []string{plugin}}.Encode()
	}
	return u.String(), nil
}

// ssPlugin 把 Clash 的 plugin/plugin-opts 转成 SIP003 的 plugin 参数。
// 本程序不实现插件传输，这里只保证分享链接不丢失信息。
func ssPlugin(raw map[string]any) string {
	name := toString(raw["plugin"])
	if name == "" {
		return ""
	}
	opts := map[string]any{}
	if m, ok := raw["plugin-opts"].(map[string]any); ok {
		opts = m
	}
	switch name {
	case "obfs":
		parts := []string{"obfs-local"}
		if mode := toString(opts["mode"]); mode != "" {
			parts = append(parts, "obfs="+mode)
		}
		if host := toString(opts["host"]); host != "" {
			parts = append(parts, "obfs-host="+host)
		}
		return strings.Join(parts, ";")
	case "v2ray-plugin":
		parts := []string{"v2ray-plugin"}
		mode := toString(opts["mode"])
		if mode == "" {
			mode = "websocket"
		}
		parts = append(parts, "mode="+mode)
		if toString(opts["tls"]) == "true" {
			parts = append(parts, "tls")
		}
		if host := toString(opts["host"]); host != "" {
			parts = append(parts, "host="+host)
		}
		if path := toString(opts["path"]); path != "" {
			parts = append(parts, "path="+path)
		}
		return strings.Join(parts, ";")
	default:
		return name
	}
}

// trojanLink 输出 trojan://password@host:port?security=tls&...#name
func trojanLink(p Proxy) (string, error) {
	password := toString(p.Raw["password"])
	if password == "" {
		return "", fmt.Errorf("trojan node missing password")
	}
	q := url.Values{}
	q.Set("security", "tls")
	if sni := firstNonEmpty(toString(p.Raw["sni"]), toString(p.Raw["servername"])); sni != "" {
		q.Set("sni", sni)
	}
	if booleanish(p.Raw["skip-cert-verify"]) {
		// v2rayN 系用 allowInsecure，v2rayA 两者都认。
		q.Set("allowInsecure", "1")
	}
	applyTransport(q, p.Raw)

	u := &url.URL{
		Scheme:   "trojan",
		Host:     hostPort(p),
		RawQuery: q.Encode(),
		Fragment: p.Name,
	}
	// trojan 的口令就是 userinfo 本身。url.User 不会转义冒号，而客户端多按
	// 「@ 之前整段是用户名」解析，口令含冒号时会被截断，这里补一次编码。
	userinfo := strings.ReplaceAll(url.User(password).String(), ":", "%3A")
	return u.Scheme + "://" + userinfo + "@" + u.Host + "?" + u.RawQuery + "#" + u.EscapedFragment(), nil
}

// vmessLink 输出 v2rayN 的 vmess://base64(JSON) 格式。
func vmessLink(p Proxy) (string, error) {
	id := toString(p.Raw["uuid"])
	if id == "" {
		return "", fmt.Errorf("vmess node missing uuid")
	}

	netw := strings.ToLower(toString(p.Raw["network"]))
	if netw == "" {
		netw = "tcp"
	}
	useTLS := booleanish(p.Raw["tls"])

	sni := ""
	if useTLS {
		sni = firstNonEmpty(toString(p.Raw["servername"]), toString(p.Raw["sni"]), p.Server)
	}

	host, path := wsHostPath(p.Raw)

	payload := vmessPayload{
		V:    "2",
		PS:   p.Name,
		Add:  p.Server,
		Port: strconv.Itoa(p.Port),
		ID:   id,
		AID:  strconv.Itoa(toInt(p.Raw["alterId"])),
		Net:  netw,
		Type: "none",
		Host: host,
		Path: path,
	}
	// auto 不能直接下发：部分内核只认具体算法，null 时干脆不写 scy。
	if scy := vmessSecurityName(toString(p.Raw["cipher"])); scy != "" {
		payload.Scy = scy
	}
	if useTLS {
		payload.TLS = "tls"
		payload.SNI = sni
	}
	if alpn := toString(p.Raw["alpn"]); alpn != "" && useTLS {
		payload.Alpn = alpn
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(body), nil
}

type vmessPayload struct {
	V    string `json:"v"`
	PS   string `json:"ps"`
	Add  string `json:"add"`
	Port string `json:"port"`
	ID   string `json:"id"`
	AID  string `json:"aid"`
	Scy  string `json:"scy,omitempty"`
	Net  string `json:"net"`
	Type string `json:"type"`
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls,omitempty"`
	SNI  string `json:"sni,omitempty"`
	Alpn string `json:"alpn,omitempty"`
}

// vmessSecurityName 把 Clash 的 cipher 映射为分享链接里的 scy。
// 返回空字符串表示让客户端用默认值。
func vmessSecurityName(cipher string) string {
	switch strings.ToLower(cipher) {
	case "", "auto":
		return ""
	case "aes-128-gcm", "chacha20-poly1305", "none", "zero":
		return strings.ToLower(cipher)
	default:
		return ""
	}
}

// applyTransport 为 trojan 链接补充 ws 传输参数。
func applyTransport(q url.Values, raw map[string]any) {
	netw := strings.ToLower(toString(raw["network"]))
	if netw == "" {
		netw = "tcp"
	}
	q.Set("type", netw)
	if netw != "ws" {
		return
	}
	host, path := wsHostPath(raw)
	if host != "" {
		q.Set("host", host)
	}
	if path != "" {
		q.Set("path", path)
	}
}

// wsHostPath 读取 Clash 的 ws-opts.headers.Host 与 ws-opts.path。
func wsHostPath(raw map[string]any) (host, path string) {
	ws, ok := raw["ws-opts"].(map[string]any)
	if !ok {
		return "", ""
	}
	path = toString(ws["path"])
	if headers, ok := ws["headers"].(map[string]any); ok {
		for k, v := range headers {
			if strings.EqualFold(k, "host") {
				host = toString(v)
				break
			}
		}
	}
	return host, path
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// booleanish 兼容 YAML 里 true 与 "true" 两种写法。
func booleanish(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true") || t == "1"
	default:
		return false
	}
}
