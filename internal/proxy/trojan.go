package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

// Trojan 是 Trojan 协议客户端（TLS 之上的自定义密码认证），支持 tcp / ws 传输。
type Trojan struct {
	server     string
	port       int
	password   string
	sni        string
	skipVerify bool
	useTLS     bool

	network   string
	wsPath    string
	wsHost    string
	wsHeaders map[string]any
}

func newTrojan(raw map[string]any) (Dialer, error) {
	server, port := str(raw["server"]), num(raw["port"])
	password := str(raw["password"])
	if server == "" || port == 0 || password == "" {
		return nil, fmt.Errorf("%w: trojan node missing server/port/password", ErrUnsupported)
	}
	network := strings.ToLower(str(raw["network"]))
	if network == "" {
		network = "tcp"
	}
	if network != "tcp" && network != "ws" {
		return nil, fmt.Errorf("%w: trojan network=%s", ErrUnsupported, network)
	}
	sni := str(raw["sni"])
	if sni == "" {
		sni = str(raw["servername"])
	}
	if sni == "" {
		sni = server
	}
	t := &Trojan{
		server:     server,
		port:       port,
		password:   password,
		sni:        sni,
		skipVerify: skipVerifyFrom(raw),
		useTLS:     true,
		network:    network,
	}
	if ws := subMap(raw["ws-opts"]); ws != nil {
		t.wsPath = str(ws["path"])
		t.wsHeaders = subMap(ws["headers"])
		t.wsHost = wsHostOf(t.wsHeaders)
	}
	return t, nil
}

func (t *Trojan) String() string { return fmt.Sprintf("trojan[%s] %s:%d", t.network, t.server, t.port) }

func (t *Trojan) Dial(ctx context.Context, target string) (net.Conn, error) {
	raw, err := dialTCP(ctx, t.server, t.port)
	if err != nil {
		return nil, err
	}

	var conn net.Conn = raw
	if t.useTLS {
		tlsConn := tls.Client(raw, &tls.Config{
			ServerName:         t.sni,
			InsecureSkipVerify: t.skipVerify,
			NextProtos:         []string{"http/1.1"},
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("trojan: tls handshake: %w", err)
		}
		conn = tlsConn
	}

	if t.network == "ws" {
		host := t.wsHost
		if host == "" {
			host = t.sni
		}
		ws, err := newWSConn(conn, host, t.wsPath, t.wsHeaders)
		if err != nil {
			conn.Close()
			return nil, err
		}
		conn = ws
	}

	// 请求头：hex(sha224(password)) CRLF CMD ATYP ADDR PORT CRLF
	sum := sha256.Sum224([]byte(t.password))
	var b bytes.Buffer
	b.WriteString(hex.EncodeToString(sum[:]))
	b.WriteString("\r\n")
	b.WriteByte(0x01) // CONNECT
	if err := EncodeSOCKS5Addr(&b, target); err != nil {
		conn.Close()
		return nil, err
	}
	b.WriteString("\r\n")

	if _, err := conn.Write(b.Bytes()); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
