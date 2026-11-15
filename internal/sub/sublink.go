package sub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// 订阅文件有三种常见形态，站点上的「通用订阅」基本都属于后两种：
//
//  1. Clash YAML（proxies: 段）
//  2. base64 编码的分享链接列表（vmess:// / trojan:// / ss:// ...）
//  3. 明文分享链接，一行一个
//
// Parse 只处理第 1 种；ParseAny 会依次尝试三种，并兼容 base64 包裹的 YAML。

// ErrUnsupportedLink 表示该分享链接的协议本程序不实现（如 vless、hysteria2）。
var ErrUnsupportedLink = errors.New("unsupported link")

// ParseAny 尽力解析任意订阅格式，返回其中的节点。
// 不支持探测的协议会被静默丢弃，因为它们本来也无法验证可用性。
func ParseAny(data []byte) ([]Proxy, error) {
	text := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if text == "" {
		return nil, fmt.Errorf("订阅内容为空")
	}

	// 1. 直接当 Clash YAML。
	if nodes, err := parseClashYAML([]byte(text)); err == nil && len(nodes) > 0 {
		return nodes, nil
	}

	// 2. 直接当分享链接文本。
	if nodes := ParseLinks(text); len(nodes) > 0 {
		return nodes, nil
	}

	// 3. base64 包裹后再试。既可能是链接列表，也可能是 Clash YAML。
	for _, decoded := range decodeBase64Variants(text) {
		body := strings.TrimSpace(strings.TrimPrefix(decoded, "\ufeff"))
		if body == "" {
			continue
		}
		if nodes, err := parseClashYAML([]byte(body)); err == nil && len(nodes) > 0 {
			return nodes, nil
		}
		if nodes := ParseLinks(body); len(nodes) > 0 {
			return nodes, nil
		}
	}

	return nil, fmt.Errorf("无法识别的订阅格式（既不是 Clash YAML，也不是分享链接列表）")
}

// ParseLinks 逐行解析分享链接，跳过空行、注释与不支持的协议。
func ParseLinks(text string) []Proxy {
	var out []Proxy
	seen := make(map[string]struct{}, 64)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "://") {
			continue
		}
		p, err := ParseLink(line)
		if err != nil {
			continue
		}
		key := p.Key()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

// ParseLink 把单条分享链接还原成 Clash 风格的节点定义，是 Link 的逆过程。
func ParseLink(link string) (Proxy, error) {
	link = strings.TrimSpace(link)
	lower := strings.ToLower(link)
	switch {
	case strings.HasPrefix(lower, "ss://"):
		return parseSSLink(link)
	case strings.HasPrefix(lower, "trojan://"):
		return parseTrojanLink(link)
	case strings.HasPrefix(lower, "vmess://"):
		return parseVMessLink(link)
	case strings.HasPrefix(lower, "vless://"):
		return parseVLESSLink(link)
	default:
		return Proxy{}, fmt.Errorf("%w: %s", ErrUnsupportedLink, schemeOf(link))
	}
}

func schemeOf(link string) string {
	if i := strings.Index(link, "://"); i > 0 {
		return link[:i]
	}
	return ""
}

// decodeBase64Variants 依次尝试标准 / URL 安全 / 无填充几种编码。
func decodeBase64Variants(s string) []string {
	var out []string
	compact := strings.Join(strings.Fields(s), "") // base64 里可能有换行
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(compact); err == nil && len(decoded) > 0 {
			out = append(out, string(decoded))
		}
	}
	return out
}

func decodeBase64String(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(s); err == nil && len(decoded) > 0 {
			return string(decoded), true
		}
	}
	return "", false
}

// splitLinkName 拆出 #后面的节点名（已解码）与主体。
func splitLinkName(raw string) (body, name string) {
	body = raw
	if i := strings.IndexByte(body, '#'); i >= 0 {
		name = percentDecode(body[i+1:])
		body = body[:i]
	}
	return body, name
}

func percentDecode(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

// parseHostPort 解析 host:port，兼容 IPv6 的方括号写法。
func parseHostPort(s string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		// 少数老订阅省略方括号，按最后一个冒号切分。
		i := strings.LastIndexByte(s, ':')
		if i <= 0 {
			return "", 0, fmt.Errorf("地址缺少端口: %s", s)
		}
		host, portStr = strings.Trim(s[:i], "[]"), s[i+1:]
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("端口无效: %s", s)
	}
	return strings.Trim(host, "[]"), port, nil
}

// parseSSLink 支持三种写法：
//
//	ss://base64url(method:password)@host:port?plugin=...#name   （SIP002）
//	ss://base64(method:password@host:port)#name                 （旧版整体编码）
//	ss://method:password@host:port#name                          （明文）
func parseSSLink(link string) (Proxy, error) {
	body, name := splitLinkName(strings.TrimPrefix(link, "ss://"))
	body, query := splitQuery(body)

	var methodPassword, hostport string
	if at := strings.LastIndex(body, "@"); at >= 0 {
		userinfo, hp := body[:at], body[at+1:]
		cred := percentDecode(userinfo)
		if decoded, ok := decodeBase64String(userinfo); ok {
			cred = decoded
		}
		methodPassword, hostport = cred, hp
	} else {
		decoded, ok := decodeBase64String(body)
		if !ok {
			return Proxy{}, fmt.Errorf("%w: ss 链接缺少主机信息", ErrUnsupportedLink)
		}
		at := strings.LastIndex(decoded, "@")
		if at < 0 {
			return Proxy{}, fmt.Errorf("%w: ss 链接格式不正确", ErrUnsupportedLink)
		}
		methodPassword, hostport = decoded[:at], decoded[at+1:]
	}

	method, password, ok := strings.Cut(methodPassword, ":")
	if !ok || method == "" || password == "" {
		return Proxy{}, fmt.Errorf("%w: ss 凭证不完整", ErrUnsupportedLink)
	}

	q, _ := url.ParseQuery(query)
	if q.Get("plugin") != "" {
		// 插件（obfs / v2ray-plugin）本程序不实现，探测必然失败，直接跳过。
		return Proxy{}, fmt.Errorf("%w: ss plugin", ErrUnsupportedLink)
	}

	host, port, err := parseHostPort(hostport)
	if err != nil {
		return Proxy{}, err
	}

	raw := map[string]any{
		"name":     nameOrDefault(name, host, port),
		"type":     "ss",
		"server":   host,
		"port":     port,
		"cipher":   strings.ToLower(method),
		"password": password,
	}
	return newProxy(raw), nil
}

// parseTrojanLink 解析 trojan://password@host:port?security=tls&type=ws&...
func parseTrojanLink(link string) (Proxy, error) {
	u, err := url.Parse(link)
	if err != nil {
		return Proxy{}, fmt.Errorf("%w: trojan 链接无法解析: %v", ErrUnsupportedLink, err)
	}
	password := u.User.Username()
	if password == "" {
		return Proxy{}, fmt.Errorf("%w: trojan 缺少密码", ErrUnsupportedLink)
	}
	host, port, err := parseHostPort(u.Host)
	if err != nil {
		return Proxy{}, err
	}

	q := u.Query()
	security := strings.ToLower(q.Get("security"))
	if security != "" && security != "tls" && security != "xtls" {
		return Proxy{}, fmt.Errorf("%w: trojan security=%s", ErrUnsupportedLink, security)
	}

	network := strings.ToLower(q.Get("type"))
	if network == "" {
		network = "tcp"
	}

	raw := map[string]any{
		"name":     nameOrDefault(u.Fragment, host, port),
		"type":     "trojan",
		"server":   host,
		"port":     port,
		"password": password,
	}
	if network != "tcp" {
		raw["network"] = network
	}
	if sni := q.Get("sni"); sni != "" {
		raw["sni"] = sni
	} else if peer := q.Get("peer"); peer != "" {
		raw["sni"] = peer
	}
	if v := q.Get("allowInsecure"); v == "1" || strings.EqualFold(v, "true") {
		raw["skip-cert-verify"] = true
	}
	if network == "ws" {
		ws := map[string]any{}
		if path := q.Get("path"); path != "" {
			ws["path"] = path
		}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		if len(ws) > 0 {
			raw["ws-opts"] = ws
		}
	}
	return newProxy(raw), nil
}

// parseVMessLink 解析 vmess://base64(JSON)，v2rayN 的格式。
func parseVMessLink(link string) (Proxy, error) {
	body := strings.TrimPrefix(link, "vmess://")
	decoded, ok := decodeBase64String(body)
	if !ok {
		return Proxy{}, fmt.Errorf("%w: vmess 主体不是 base64", ErrUnsupportedLink)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(decoded), &m); err != nil {
		return Proxy{}, fmt.Errorf("%w: vmess JSON 无法解析: %v", ErrUnsupportedLink, err)
	}

	host := toString(m["add"])
	port := toInt(m["port"])
	uuid := toString(m["id"])
	if host == "" || port == 0 || uuid == "" {
		return Proxy{}, fmt.Errorf("%w: vmess 缺少 add/port/id", ErrUnsupportedLink)
	}

	network := strings.ToLower(toString(m["net"]))
	if network == "" {
		network = "tcp"
	}

	raw := map[string]any{
		"name":    nameOrDefault(toString(m["ps"]), host, port),
		"type":    "vmess",
		"server":  host,
		"port":    port,
		"uuid":    uuid,
		"alterId": toInt(m["aid"]),
		"cipher":  vmessCipher(toString(m["scy"])),
	}
	if network != "tcp" {
		raw["network"] = network
	}
	if strings.EqualFold(toString(m["tls"]), "tls") {
		raw["tls"] = true
		sni := toString(m["sni"])
		if sni == "" {
			sni = toString(m["host"])
		}
		if sni == "" {
			sni = host
		}
		raw["servername"] = sni
	}
	if network == "ws" {
		ws := map[string]any{}
		if path := toString(m["path"]); path != "" {
			ws["path"] = path
		}
		if h := toString(m["host"]); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		if len(ws) > 0 {
			raw["ws-opts"] = ws
		}
	}
	return newProxy(raw), nil
}

// parseVLESSLink 解析 vless://uuid@host:port?security=tls&type=ws&...
//
// 免费节点池里 VLESS 占比不低，且绝大多数是普通 TLS + WS。REALITY 需要
// 伪造特定 ClientHello 指纹，本程序不实现，直接判为不支持。
func parseVLESSLink(link string) (Proxy, error) {
	u, err := url.Parse(link)
	if err != nil {
		return Proxy{}, fmt.Errorf("%w: vless 链接无法解析: %v", ErrUnsupportedLink, err)
	}
	uuid := u.User.Username()
	if uuid == "" {
		return Proxy{}, fmt.Errorf("%w: vless 缺少 uuid", ErrUnsupportedLink)
	}
	host, port, err := parseHostPort(u.Host)
	if err != nil {
		return Proxy{}, err
	}

	q := u.Query()
	security := strings.ToLower(q.Get("security"))
	switch security {
	case "", "none", "tls":
	case "reality":
		return Proxy{}, fmt.Errorf("%w: vless reality", ErrUnsupportedLink)
	default:
		return Proxy{}, fmt.Errorf("%w: vless security=%s", ErrUnsupportedLink, security)
	}

	network := strings.ToLower(q.Get("type"))
	switch network {
	case "", "tcp", "raw", "none":
		network = "tcp"
	case "ws":
	default:
		return Proxy{}, fmt.Errorf("%w: vless network=%s", ErrUnsupportedLink, network)
	}

	raw := map[string]any{
		"name":   nameOrDefault(u.Fragment, host, port),
		"type":   "vless",
		"server": host,
		"port":   port,
		"uuid":   uuid,
	}
	if network != "tcp" {
		raw["network"] = network
	}
	if security == "tls" {
		raw["tls"] = true
		sni := q.Get("sni")
		if sni == "" {
			sni = q.Get("host")
		}
		if sni == "" {
			sni = host
		}
		raw["servername"] = sni
	}
	if v := q.Get("allowInsecure"); v == "1" || strings.EqualFold(v, "true") {
		raw["skip-cert-verify"] = true
	}
	if alpn := q.Get("alpn"); alpn != "" {
		raw["alpn"] = percentDecode(alpn)
	}
	if flow := q.Get("flow"); flow != "" {
		// xtls-rprx-vision 的填充只作用于 TLS-in-TLS 流量，普通请求按原样透传，
		// 因此这里保留声明即可，探测结果会自然筛掉不兼容的节点。
		raw["flow"] = flow
	}
	if network == "ws" {
		ws := map[string]any{}
		if path := q.Get("path"); path != "" {
			// 部分订阅把 path 编码了两层（%252F...），要再解一次才是 /api/...
			ws["path"] = unescapeLayers(path, 2)
		}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		if len(ws) > 0 {
			raw["ws-opts"] = ws
		}
	}
	return newProxy(raw), nil
}

// unescapeLayers 最多解码 max 层百分号编码。
func unescapeLayers(s string, max int) string {
	for i := 0; i < max && strings.Contains(s, "%"); i++ {
		d, err := url.PathUnescape(s)
		if err != nil || d == s {
			break
		}
		s = d
	}
	return s
}

// vmessCipher 把分享链接里的 scy 映射回 Clash 的 cipher 取值。
func vmessCipher(scy string) string {
	switch strings.ToLower(scy) {
	case "aes-128-gcm", "chacha20-poly1305", "none", "zero":
		return strings.ToLower(scy)
	default:
		return "auto"
	}
}

// newProxy 由归一化后的字段构造节点，Name/Type/Server/Port 同时冗余出来，
// 避免调用方反复从 map 里取值。
func newProxy(raw map[string]any) Proxy {
	return Proxy{
		Name:   toString(raw["name"]),
		Type:   strings.ToLower(toString(raw["type"])),
		Server: toString(raw["server"]),
		Port:   toInt(raw["port"]),
		Raw:    raw,
	}
}

func nameOrDefault(name, host string, port int) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func splitQuery(s string) (body, query string) {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
