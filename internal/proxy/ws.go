package proxy

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// 实现 RFC 6455 的最小 WebSocket 客户端（仅二进制帧、客户端掩码）。
// 免费订阅中的 VMess 节点绝大多数通过 WebSocket 传输，因此这一层是必需的。

const wsAcceptSalt = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	net.Conn
	br *bufio.Reader

	wmu  sync.Mutex
	rbuf []byte
}

// newWSConn 在已建立的字节流上完成 WebSocket 升级握手。
func newWSConn(conn net.Conn, host, path string, headers map[string]any) (*wsConn, error) {
	if path == "" {
		path = "/"
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&sb, "Host: %s\r\n", host)
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&sb, "Sec-WebSocket-Key: %s\r\n", key)
	sb.WriteString("Sec-WebSocket-Version: 13\r\n")
	sb.WriteString("User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64)\r\n")
	for k, v := range headers {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Upgrade") ||
			strings.EqualFold(k, "Connection") || strings.HasPrefix(strings.ToLower(k), "sec-websocket") {
			continue
		}
		fmt.Fprintf(&sb, "%s: %s\r\n", k, str(v))
	}
	sb.WriteString("\r\n")

	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(conn, 4096)
	req := &http.Request{Method: http.MethodGet}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("ws: read handshake response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("ws: unexpected status %s", resp.Status)
	}

	sum := sha1.Sum([]byte(key + wsAcceptSalt))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "" && got != want {
		return nil, fmt.Errorf("ws: bad accept key")
	}
	return &wsConn{Conn: conn, br: br}, nil
}

func (c *wsConn) Read(p []byte) (int, error) {
	for len(c.rbuf) == 0 {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return 0, err
		}
		switch opcode {
		case 0x8: // close
			return 0, io.EOF
		case 0x9: // ping -> pong
			_ = c.writeFrame(0xA, payload)
		case 0xA: // pong，忽略
		default: // 0x0 续帧 / 0x1 文本 / 0x2 二进制
			c.rbuf = payload
		}
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *wsConn) Write(p []byte) (int, error) {
	const maxFrame = 32 * 1024
	total := 0
	for total < len(p) {
		n := len(p) - total
		if n > maxFrame {
			n = maxFrame
		}
		if err := c.writeFrame(0x2, p[total:total+n]); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|opcode)

	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, 0x80|byte(l))
	case l < 65536:
		hdr = append(hdr, 0x80|126, byte(l>>8), byte(l))
	default:
		hdr = append(hdr, 0x80|127,
			0, 0, 0, 0,
			byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)

	frame := make([]byte, 0, len(hdr)+l)
	frame = append(frame, hdr...)
	masked := make([]byte, l)
	for i := 0; i < l; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	frame = append(frame, masked...)

	_, err := c.Conn.Write(frame)
	return err
}

func (c *wsConn) readFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0

	l := int(head[1] & 0x7F)
	switch l {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		l = int(ext[0])<<8 | int(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		l = int(ext[4])<<24 | int(ext[5])<<16 | int(ext[6])<<8 | int(ext[7])
	}
	if l < 0 || l > 64*1024*1024 {
		return 0, nil, fmt.Errorf("ws: frame too large: %d", l)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, l)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}
