// Package proxy 实现多种代理协议的客户端握手，使本程序可以在自身进程内
// 建立一条通往目标地址的隧道，从而对节点做真实可用性探测（而非仅测端口连通性）。
package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ErrUnsupported 表示该节点使用了本程序尚未实现的协议或传输方式。
var ErrUnsupported = errors.New("unsupported proxy")

// defaultInsecure 为全局缺省值：免费节点普遍使用自签证书，默认放宽校验才能测通。
var defaultInsecure = true

// SetDefaultInsecure 设置是否忽略节点侧 TLS 证书错误（节点自身显式声明的配置优先）。
func SetDefaultInsecure(v bool) { defaultInsecure = v }

func skipVerifyFrom(raw map[string]any) bool {
	if v, ok := raw["skip-cert-verify"]; ok {
		return boolean(v)
	}
	return defaultInsecure
}

// Dialer 是一条可以穿透到任意目标地址的代理隧道。
type Dialer interface {
	// Dial 建立到 target（形如 host:port）的隧道连接。
	// 返回的 net.Conn 语义上等同于一条直连 target 的 TCP 连接。
	Dial(ctx context.Context, target string) (net.Conn, error)
}

// New 依据 Clash 订阅中的单个 proxies 条目构造对应协议的 Dialer。
func New(raw map[string]any) (Dialer, error) {
	switch strings.ToLower(str(raw["type"])) {
	case "ss":
		return newSS(raw)
	case "trojan":
		return newTrojan(raw)
	case "vmess":
		return newVMess(raw)
	default:
		return nil, fmt.Errorf("%w: type=%s", ErrUnsupported, str(raw["type"]))
	}
}

// ---- 通用工具 ----

func dialTCP(ctx context.Context, server string, port int) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(server, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	// 让后续的协议握手也受调用方 context 的超时约束。
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	return conn, nil
}

// EncodeSOCKS5Addr 按 SOCKS5 地址格式写入（SS / Trojan 使用）。
// 格式：ATYP(1) + ADDR + PORT(2, big endian)，ATYP: 1=IPv4 3=Domain 4=IPv6
func EncodeSOCKS5Addr(buf *bytes.Buffer, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf.WriteByte(0x01)
			buf.Write(ip4)
		} else {
			buf.WriteByte(0x04)
			buf.Write(ip.To16())
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("domain too long: %s", host)
		}
		buf.WriteByte(0x03)
		buf.WriteByte(byte(len(host)))
		buf.WriteString(host)
	}
	writePort(buf, port)
	return nil
}

// EncodeVMessAddr 按 VMess 地址格式写入。
// 注意两点与 SOCKS5 不同：
//  1. 顺序为 PORT(2) + ATYP(1) + ADDR，端口在地址之前（对应 PortThenAddress）；
//  2. ATYP 取值不同：1=IPv4 2=Domain 3=IPv6（域名与 IPv6 与 SOCKS5 相反）。
func EncodeVMessAddr(buf *bytes.Buffer, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	writePort(buf, port)
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf.WriteByte(0x01)
			buf.Write(ip4)
		} else {
			buf.WriteByte(0x03)
			buf.Write(ip.To16())
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("domain too long: %s", host)
		}
		buf.WriteByte(0x02)
		buf.WriteByte(byte(len(host)))
		buf.WriteString(host)
	}
	return nil
}

func writePort(buf *bytes.Buffer, port int) {
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	buf.Write(p[:])
}

func splitTarget(target string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port in %q", target)
	}
	return host, port, nil
}

// ---- 配置字段读取辅助 ----

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func num(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	default:
		return 0
	}
}

func boolean(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true") || t == "1"
	default:
		return false
	}
}

func subMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// wsHostOf 从 ws-opts.headers 中大小写不敏感地取出 Host。
func wsHostOf(headers map[string]any) string {
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			return str(v)
		}
	}
	return ""
}
