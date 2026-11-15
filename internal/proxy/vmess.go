package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"net"
	"strings"
	"time"

	"crypto/hmac"
	"crypto/md5"
)

// VMess 是 VMess(AEAD) 客户端，支持 tcp / ws 传输与可选 TLS。
type VMess struct {
	server     string
	port       int
	uuid       [16]byte
	cmdKey     [16]byte
	security   byte
	useTLS     bool
	sni        string
	skipVerify bool
	network    string
	wsPath     string
	wsHost     string
	wsHeaders  map[string]any
}

const (
	securityAES128GCM  byte = 3
	securityChaChaPoly byte = 4
	securityNone       byte = 5
)

func newVMess(raw map[string]any) (Dialer, error) {
	server, port := str(raw["server"]), num(raw["port"])
	if server == "" || port == 0 {
		return nil, fmt.Errorf("%w: vmess node missing server/port", ErrUnsupported)
	}
	id, err := parseUUID(str(raw["uuid"]))
	if err != nil {
		return nil, fmt.Errorf("%w: vmess uuid: %v", ErrUnsupported, err)
	}
	if alterID := num(raw["alterId"]); alterID > 0 {
		// alterId > 0 属于旧版 VMess（非 AEAD），本程序不实现该加密路径。
		return nil, fmt.Errorf("%w: vmess alterId=%d (legacy)", ErrUnsupported, alterID)
	}

	network := strings.ToLower(str(raw["network"]))
	if network == "" {
		network = "tcp"
	}
	if network != "tcp" && network != "ws" {
		return nil, fmt.Errorf("%w: vmess network=%s", ErrUnsupported, network)
	}

	sni := str(raw["servername"])
	if sni == "" {
		sni = str(raw["sni"])
	}
	if sni == "" {
		sni = server
	}

	v := &VMess{
		server:     server,
		port:       port,
		uuid:       id,
		cmdKey:     vmessCmdKey(id),
		security:   vmessSecurity(str(raw["cipher"])),
		useTLS:     boolean(raw["tls"]),
		sni:        sni,
		skipVerify: skipVerifyFrom(raw),
		network:    network,
	}
	if ws := subMap(raw["ws-opts"]); ws != nil {
		v.wsPath = str(ws["path"])
		v.wsHeaders = subMap(ws["headers"])
		v.wsHost = wsHostOf(v.wsHeaders)
	}
	return v, nil
}

func (v *VMess) String() string { return fmt.Sprintf("vmess[%s] %s:%d", v.network, v.server, v.port) }

func (v *VMess) Dial(ctx context.Context, target string) (net.Conn, error) {
	raw, err := dialTCP(ctx, v.server, v.port)
	if err != nil {
		return nil, err
	}

	var conn net.Conn = raw
	if v.useTLS {
		tlsConn := tls.Client(raw, &tls.Config{
			ServerName:         v.sni,
			InsecureSkipVerify: v.skipVerify,
			NextProtos:         []string{"http/1.1"},
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("vmess: tls handshake: %w", err)
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

	vc, err := v.handshake(conn, target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return vc, nil
}

// handshake 完成 VMess AEAD 请求头封装。
//
// 注意：响应头不能在此处读取。Xray 服务端会等到目标真正有数据返回时才下发响应头，
// 若在此阻塞等待，就会与「先建隧道、再发请求」的调用方形成死锁。
// 因此响应头的解析推迟到首次 Read。
func (v *VMess) handshake(conn net.Conn, target string) (*vmessConn, error) {
	var reqBodyKey, reqBodyIV [16]byte
	var respHeaderByte [1]byte
	if _, err := rand.Read(reqBodyKey[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(reqBodyIV[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(respHeaderByte[:]); err != nil {
		return nil, err
	}

	paddingLen := int(respHeaderByte[0] & 0x0F)
	var pad [16]byte
	if _, err := rand.Read(pad[:paddingLen]); err != nil {
		return nil, err
	}

	// 明文请求头，字段顺序与 v2ray-core 完全一致。
	var hdr bytes.Buffer
	hdr.WriteByte(1) // version
	hdr.Write(reqBodyIV[:])
	hdr.Write(reqBodyKey[:])
	hdr.WriteByte(respHeaderByte[0])
	hdr.WriteByte(0x01) // option: RequestOptionChunkStream
	hdr.WriteByte(byte(paddingLen<<4) | v.security)
	hdr.WriteByte(0x00) // reserved
	hdr.WriteByte(0x01) // command: TCP
	if err := EncodeVMessAddr(&hdr, target); err != nil {
		return nil, err
	}
	hdr.Write(pad[:paddingLen])
	checksum := fnv.New32a()
	checksum.Write(hdr.Bytes())
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], checksum.Sum32())
	hdr.Write(sum[:])

	if _, err := conn.Write(sealVMessAEADHeader(v.cmdKey, hdr.Bytes())); err != nil {
		return nil, fmt.Errorf("vmess: write request header: %w", err)
	}

	// 响应体密钥由请求体密钥派生。
	sumKey := sha256.Sum256(reqBodyKey[:])
	sumIV := sha256.Sum256(reqBodyIV[:])
	respBodyKey := sumKey[:16]
	respBodyIV := sumIV[:16]

	lenAEAD, err := newGCM(vmessKDF16(respBodyKey, "AEAD Resp Header Len Key"))
	if err != nil {
		return nil, err
	}
	payAEAD, err := newGCM(vmessKDF16(respBodyKey, "AEAD Resp Header Key"))
	if err != nil {
		return nil, err
	}

	wAEAD, err := newGCM(reqBodyKey[:])
	if err != nil {
		return nil, err
	}
	rAEAD, err := newGCM(respBodyKey)
	if err != nil {
		return nil, err
	}

	return &vmessConn{
		Conn:   conn,
		br:     bufio.NewReaderSize(conn, 4096),
		w:      wAEAD,
		wNonce: append([]byte(nil), reqBodyIV[:12]...),
		r:      rAEAD,
		rNonce: append([]byte(nil), respBodyIV[:12]...),

		respPending: true,
		lenAEAD:     lenAEAD,
		lenIV:       vmessKDF(respBodyIV, "AEAD Resp Header Len IV")[:12],
		payAEAD:     payAEAD,
		payIV:       vmessKDF(respBodyIV, "AEAD Resp Header IV")[:12],
		wantResp:    respHeaderByte[0],
	}, nil
}

// vmessConn 实现 VMess 的数据帧（AES-128-GCM，2 字节长度字段与载荷各自封装）。
type vmessConn struct {
	net.Conn
	w, r           cipher.AEAD
	wNonce, rNonce []byte
	wcount, rcount uint16
	br             *bufio.Reader
	rbuf           []byte

	// 响应头延迟解析
	respPending bool
	lenAEAD     cipher.AEAD
	lenIV       []byte
	payAEAD     cipher.AEAD
	payIV       []byte
	wantResp    byte
}

// vmessChunkNonce 按官方约定生成帧 nonce：前 2 字节为大端计数器，其余沿用 IV。
func vmessChunkNonce(nonce []byte, count uint16) []byte {
	binary.BigEndian.PutUint16(nonce, count)
	return nonce[:12]
}

func (c *vmessConn) Write(p []byte) (int, error) {
	const maxChunk = 16384
	written := 0
	for written < len(p) {
		n := len(p) - written
		if n > maxChunk {
			n = maxChunk
		}
		var lenBuf [2]byte
		sealedPayload := c.w.Seal(nil, vmessChunkNonce(c.wNonce, c.wcount), p[written:written+n], nil)
		c.wcount++
		// 未启用 AuthenticatedLength 时，长度字段是明文，取值为密文长度。
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(sealedPayload)))

		if _, err := c.Conn.Write(lenBuf[:]); err != nil {
			return written, err
		}
		if _, err := c.Conn.Write(sealedPayload); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (c *vmessConn) Read(p []byte) (int, error) {
	if c.respPending {
		c.respPending = false
		if err := c.readResponseHeader(); err != nil {
			return 0, err
		}
	}
	for len(c.rbuf) == 0 {
		var lenBuf [2]byte
		if _, err := io.ReadFull(c.br, lenBuf[:]); err != nil {
			return 0, err
		}
		// 长度字段是密文长度（含 16 字节认证标签）。
		size := int(binary.BigEndian.Uint16(lenBuf[:]))
		if size < 16 {
			return 0, fmt.Errorf("vmess: invalid chunk size %d", size)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			return 0, err
		}
		plain, err := c.r.Open(buf[:0], vmessChunkNonce(c.rNonce, c.rcount), buf, nil)
		if err != nil {
			return 0, fmt.Errorf("vmess: decrypt body chunk: %w", err)
		}
		c.rcount++
		if len(plain) == 0 {
			// 空载荷块是流结束标记。
			return 0, io.EOF
		}
		c.rbuf = plain
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

// readResponseHeader 解析长度被单独封装、载荷再封装的两段式响应头。
func (c *vmessConn) readResponseHeader() error {
	var encLen [18]byte
	if _, err := io.ReadFull(c.br, encLen[:]); err != nil {
		return fmt.Errorf("vmess: read response header len: %w", err)
	}
	plainLen, err := c.lenAEAD.Open(nil, c.lenIV, encLen[:], nil)
	if err != nil {
		return fmt.Errorf("vmess: decrypt response header len: %w", err)
	}
	bodyLen := int(binary.BigEndian.Uint16(plainLen))

	encPay := make([]byte, bodyLen+16)
	if _, err := io.ReadFull(c.br, encPay); err != nil {
		return fmt.Errorf("vmess: read response header: %w", err)
	}
	payload, err := c.payAEAD.Open(nil, c.payIV, encPay, nil)
	if err != nil {
		return fmt.Errorf("vmess: decrypt response header: %w", err)
	}
	if len(payload) < 1 {
		return fmt.Errorf("vmess: response header too short (%d bytes)", len(payload))
	}
	if payload[0] != c.wantResp {
		return fmt.Errorf("vmess: response header mismatch (auth failed or server error)")
	}
	if len(payload) >= 3 && payload[2] != 0 {
		return fmt.Errorf("vmess: server returned command %d", payload[2])
	}
	return nil
}

// ---- VMess 密码学原语（对齐 v2ray-core/proxy/vmess/aead）----

func parseUUID(s string) ([16]byte, error) {
	var out [16]byte
	clean := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(clean) != 32 {
		return out, fmt.Errorf("invalid uuid %q", s)
	}
	_, err := hex.Decode(out[:], []byte(clean))
	return out, err
}

// vmessCmdKey = MD5(uuid || "c48619fe-8f02-49e0-b9e9-edf763e17e21")
func vmessCmdKey(id [16]byte) [16]byte {
	buf := make([]byte, 0, 48)
	buf = append(buf, id[:]...)
	buf = append(buf, []byte("c48619fe-8f02-49e0-b9e9-edf763e17e21")...)
	return md5.Sum(buf)
}

func vmessSecurity(cipherName string) byte {
	switch strings.ToLower(cipherName) {
	case "chacha20-poly1305":
		return securityChaChaPoly
	case "none":
		return securityNone
	default: // auto / aes-128-gcm / 空值
		return securityAES128GCM
	}
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// vmessKDFSalt 是 VMess AEAD KDF 的固定盐值。
const vmessKDFSalt = "VMess AEAD KDF"

// vmessKDF 复现 v2ray / xray 的 VMess AEAD KDF。
//
// 该构造并非「上一级输出作为下一级消息」的朴素链式 HMAC，而是把上一级 HMAC
// 整体当作下一级的哈希函数，逐级嵌套：
//
//	H₀(x) = HMAC-SHA256(key="VMess AEAD KDF", msg=x)
//	Hᵢ(x) = Hᵢ₋₁(opadᵢ ‖ Hᵢ₋₁(ipadᵢ ‖ x))，其中 ipad/opad 由 pathᵢ 按 HMAC 规则生成
//	KDF(key, path...) = Hₙ(key)
//
// 两种写法结果不同，此处必须严格对齐官方实现，否则服务端无法解出 authID。
func vmessKDF(key []byte, path ...string) []byte {
	h := func(x []byte) []byte {
		m := hmac.New(sha256.New, []byte(vmessKDFSalt))
		m.Write(x)
		return m.Sum(nil)
	}
	for _, p := range path {
		prev := h
		ipad := hmacPad(p, 0x36)
		opad := hmacPad(p, 0x5c)
		h = func(x []byte) []byte {
			inner := prev(concatBytes(ipad, x))
			return prev(concatBytes(opad, inner))
		}
	}
	return h(key)
}

// hmacPad 按 HMAC 规则把密钥填充到 SHA-256 的块大小，并与给定掩码异或。
func hmacPad(key string, mask byte) []byte {
	k := []byte(key)
	if len(k) > sha256.BlockSize {
		sum := sha256.Sum256(k)
		k = sum[:]
	}
	out := make([]byte, sha256.BlockSize)
	for i := range out {
		out[i] = mask
	}
	for i := range k {
		out[i] ^= k[i]
	}
	return out
}

func concatBytes(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

func vmessKDF16(key []byte, path ...string) []byte {
	return vmessKDF(key, path...)[:16]
}

// createAuthID 生成 16 字节认证 ID：timestamp(8) + random(4) + crc32(4)，再用 AES-ECB 加密。
func createAuthID(cmdKey []byte, ts int64) [16]byte {
	var plain [16]byte
	binary.BigEndian.PutUint64(plain[:8], uint64(ts))
	rand.Read(plain[8:12])
	binary.BigEndian.PutUint32(plain[12:], crc32.ChecksumIEEE(plain[:12]))

	block, err := aes.NewCipher(vmessKDF16(cmdKey, "AES Auth ID Encryption"))
	if err != nil {
		panic(err)
	}
	var out [16]byte
	block.Encrypt(out[:], plain[:])
	return out
}

// sealVMessAEADHeader 输出：authID(16) + 加密长度(18) + 连接 nonce(8) + 加密头部。
func sealVMessAEADHeader(cmdKey [16]byte, data []byte) []byte {
	authID := createAuthID(cmdKey[:], time.Now().Unix())
	var connNonce [8]byte
	rand.Read(connNonce[:])

	authStr := string(authID[:])
	nonceStr := string(connNonce[:])

	var lenPlain [2]byte
	binary.BigEndian.PutUint16(lenPlain[:], uint16(len(data)))

	lenAEAD, err := newGCM(vmessKDF16(cmdKey[:], "VMess Header AEAD Key_Length", authStr, nonceStr))
	if err != nil {
		panic(err)
	}
	lenIV := vmessKDF(cmdKey[:], "VMess Header AEAD Nonce_Length", authStr, nonceStr)[:12]
	encLen := lenAEAD.Seal(nil, lenIV, lenPlain[:], authID[:])

	payAEAD, err := newGCM(vmessKDF16(cmdKey[:], "VMess Header AEAD Key", authStr, nonceStr))
	if err != nil {
		panic(err)
	}
	payIV := vmessKDF(cmdKey[:], "VMess Header AEAD Nonce", authStr, nonceStr)[:12]
	encPay := payAEAD.Seal(nil, payIV, data, authID[:])

	out := make([]byte, 0, 16+18+8+len(encPay))
	out = append(out, authID[:]...)
	out = append(out, encLen...)
	out = append(out, connNonce[:]...)
	out = append(out, encPay...)
	return out
}
