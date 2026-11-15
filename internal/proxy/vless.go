package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

// VLESS 是 VMess 之后的轻量协议：因为没有加密层，握手只有一段明文头部，
// 之后就是裸字节流。免费订阅里 VLESS 占比不低（尤其 GitHub 上的俄罗斯、
// 伊朗节点池），不支持它等于直接丢掉一两成节点。
//
// 本实现覆盖 TLS / WS / 明文三种传输。REALITY 需要伪造特定 ClientHello
// 指纹，超出本程序的探测需求，直接判为不支持。

// VLESS 地址类型。注意与 SOCKS5 不同：域名是 2，IPv6 是 3。
const (
	vlessAddrIPv4   = 0x01
	vlessAddrDomain = 0x02
	vlessAddrIPv6   = 0x03
	vlessCmdTCP     = 0x01
)

// VLESS 是一个 VLESS 节点的探测客户端。
type VLESS struct {
	server     string
	port       int
	uuid       [16]byte
	useTLS     bool
	sni        string
	skipVerify bool
	alpn       []string
	network    string
	wsPath     string
	wsHost     string
	wsHeaders  map[string]any
}

func newVLESS(raw map[string]any) (*VLESS, error) {
	server := str(raw["server"])
	port := num(raw["port"])
	if server == "" || port <= 0 {
		return nil, fmt.Errorf("%w: vless 缺少 server/port", ErrUnsupported)
	}

	// REALITY 的握手在 TLS 层做额外认证，普通 TLS 客户端会被直接拒绝。
	if opts := subMap(raw["reality-opts"]); len(opts) > 0 {
		return nil, fmt.Errorf("%w: vless reality", ErrUnsupported)
	}

	id, err := parseUUID(str(raw["uuid"]))
	if err != nil {
		return nil, fmt.Errorf("%w: vless uuid: %v", ErrUnsupported, err)
	}

	network := strings.ToLower(str(raw["network"]))
	switch network {
	case "", "tcp", "raw", "none":
		// "raw" 是 Xray 对 tcp 的新叫法；"none" 表示不带传输层封装。
		network = "tcp"
	case "ws":
	default:
		return nil, fmt.Errorf("%w: vless network=%s", ErrUnsupported, network)
	}

	sni := str(raw["servername"])
	if sni == "" {
		sni = str(raw["sni"])
	}
	if sni == "" {
		sni = server
	}

	v := &VLESS{
		server:     server,
		port:       port,
		uuid:       id,
		useTLS:     boolean(raw["tls"]),
		sni:        sni,
		skipVerify: skipVerifyFrom(raw),
		network:    network,
	}
	if alpn := str(raw["alpn"]); alpn != "" {
		for _, a := range strings.Split(alpn, ",") {
			if a = strings.TrimSpace(a); a != "" {
				v.alpn = append(v.alpn, a)
			}
		}
	}
	if ws := subMap(raw["ws-opts"]); ws != nil {
		v.wsPath = str(ws["path"])
		v.wsHeaders = subMap(ws["headers"])
		v.wsHost = wsHostOf(v.wsHeaders)
	}
	return v, nil
}

func (v *VLESS) String() string { return fmt.Sprintf("vless[%s] %s:%d", v.network, v.server, v.port) }

func (v *VLESS) Dial(ctx context.Context, target string) (net.Conn, error) {
	raw, err := dialTCP(ctx, v.server, v.port)
	if err != nil {
		return nil, err
	}

	var conn net.Conn = raw
	if v.useTLS {
		alpn := v.alpn
		if len(alpn) == 0 {
			// 与 WS 搭配时必须声明 http/1.1，否则部分服务端会协商成 h2。
			alpn = []string{"http/1.1"}
		}
		tlsConn := tls.Client(raw, &tls.Config{
			ServerName:         v.sni,
			InsecureSkipVerify: v.skipVerify,
			NextProtos:         alpn,
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("vless: tls handshake: %w", err)
		}
		conn = tlsConn
	}

	if v.network == "ws" {
		host := v.wsHost
		if host == "" {
			host = v.sni
		}
		ws, err := newWSConn(conn, host, v.wsPath, v.wsHeaders)
		if err != nil {
			conn.Close()
			return nil, err
		}
		conn = ws
	}

	// 请求头：版本(0) UUID 附加信息长度 附加信息 命令 端口 地址类型 地址
	var hdr bytes.Buffer
	hdr.WriteByte(0)
	hdr.Write(v.uuid[:])
	hdr.WriteByte(0) // 无附加信息
	hdr.WriteByte(vlessCmdTCP)
	if err := encodeVLESSAddr(&hdr, target); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vless: write request header: %w", err)
	}

	// 响应头同样延迟到首次 Read 再解析：服务端要等目标有数据才下发，
	// 在这里阻塞会与「先建隧道再发请求」的调用方死锁。
	return &vlessConn{Conn: conn, br: bufio.NewReaderSize(conn, 4096)}, nil
}

// encodeVLESSAddr 写入 VLESS 地址：PORT(2) + ATYP(1) + ADDR。
func encodeVLESSAddr(buf *bytes.Buffer, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	writePort(buf, port)
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf.WriteByte(vlessAddrIPv4)
			buf.Write(ip4)
		} else {
			buf.WriteByte(vlessAddrIPv6)
			buf.Write(ip.To16())
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("domain too long: %s", host)
		}
		buf.WriteByte(vlessAddrDomain)
		buf.WriteByte(byte(len(host)))
		buf.WriteString(host)
	}
	return nil
}

// vlessConn 在首次读取时消费响应头。
type vlessConn struct {
	net.Conn
	br *bufio.Reader

	mu      sync.Mutex
	hasHdr  bool
	hdrErr  error
	pending []byte
}

func (c *vlessConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if !c.hasHdr {
		c.pending, c.hdrErr = c.readHeader()
		c.hasHdr = true
	}
	pending := c.pending
	c.mu.Unlock()

	if len(pending) > 0 {
		n := copy(p, pending)
		c.mu.Lock()
		c.pending = c.pending[n:]
		c.mu.Unlock()
		return n, nil
	}
	if c.hdrErr != nil {
		return 0, c.hdrErr
	}
	return c.br.Read(p)
}

// readHeader 读取 版本(1) + 附加信息长度(1) + 附加信息。
func (c *vlessConn) readHeader() ([]byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return nil, fmt.Errorf("vless: read response header: %w", err)
	}
	if head[1] == 0 {
		return nil, nil
	}
	// 附加信息理论上不使用，出现时原样读出交给上层忽略。
	extra := make([]byte, head[1])
	if _, err := io.ReadFull(c.br, extra); err != nil {
		return nil, fmt.Errorf("vless: read response addons: %w", err)
	}
	return nil, nil
}
