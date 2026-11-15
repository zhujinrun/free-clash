package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// SS 是 Shadowsocks 客户端，同时支持 AEAD 系列与经典流加密系列。
type SS struct {
	server string
	port   int
	method string
	key    []byte
	keyLen int

	// AEAD 模式
	aead    bool
	newAEAD func(key []byte) (cipher.AEAD, error)

	// 流加密模式
	ivLen int
}

// newSS 解析 Clash 中的 ss 节点。
func newSS(raw map[string]any) (Dialer, error) {
	method := strings.ToLower(str(raw["cipher"]))
	server, port := str(raw["server"]), num(raw["port"])
	if server == "" || port == 0 {
		return nil, fmt.Errorf("%w: ss node missing server/port", ErrUnsupported)
	}

	s := &SS{server: server, port: port, method: method}
	if keyLen, fn, err := ssAEAD(method); err == nil {
		s.keyLen, s.newAEAD, s.aead = keyLen, fn, true
	} else if keyLen, ivLen, err := ssStreamParams(method); err == nil {
		s.keyLen, s.ivLen = keyLen, ivLen
	} else {
		return nil, err
	}
	s.key = evpBytesToKey(str(raw["password"]), s.keyLen)
	return s, nil
}

func ssAEAD(method string) (int, func([]byte) (cipher.AEAD, error), error) {
	aesGCM := func(keyLen int) func([]byte) (cipher.AEAD, error) {
		return func(key []byte) (cipher.AEAD, error) {
			block, err := aes.NewCipher(key)
			if err != nil {
				return nil, err
			}
			return cipher.NewGCM(block)
		}
	}
	switch method {
	case "aes-128-gcm":
		return 16, aesGCM(16), nil
	case "aes-192-gcm":
		return 24, aesGCM(24), nil
	case "aes-256-gcm":
		return 32, aesGCM(32), nil
	case "chacha20-ietf-poly1305":
		return 32, chacha20poly1305.New, nil
	default:
		return 0, nil, fmt.Errorf("%w: ss cipher %q", ErrUnsupported, method)
	}
}

func ssStreamParams(method string) (keyLen, ivLen int, err error) {
	switch method {
	case "aes-128-cfb":
		return 16, 16, nil
	case "aes-192-cfb":
		return 24, 16, nil
	case "aes-256-cfb":
		return 32, 16, nil
	case "rc4-md5":
		return 16, 16, nil
	default:
		return 0, 0, fmt.Errorf("%w: ss cipher %q", ErrUnsupported, method)
	}
}

// evpBytesToKey 复现 OpenSSL EVP_BytesToKey(MD5)，这是 Shadowsocks 由密码派生主密钥的约定。
func evpBytesToKey(password string, keyLen int) []byte {
	var material []byte
	var prev []byte
	for len(material) < keyLen {
		h := md5.New()
		h.Write(prev)
		h.Write([]byte(password))
		prev = h.Sum(nil)
		material = append(material, prev...)
	}
	return material[:keyLen]
}

func (s *SS) String() string { return fmt.Sprintf("ss[%s] %s:%d", s.method, s.server, s.port) }

func (s *SS) Dial(ctx context.Context, target string) (net.Conn, error) {
	raw, err := dialTCP(ctx, s.server, s.port)
	if err != nil {
		return nil, err
	}

	var hdr bytes.Buffer
	if err := EncodeSOCKS5Addr(&hdr, target); err != nil {
		raw.Close()
		return nil, err
	}

	if s.aead {
		c, err := newSSConn(raw, s.key, s.keyLen, s.newAEAD)
		if err != nil {
			raw.Close()
			return nil, err
		}
		// SS 的首个数据块必须以目标地址开头，服务端据此建立转发。
		if _, err := c.Write(hdr.Bytes()); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}

	c := &ssStreamConn{
		Conn:   raw,
		br:     bufio.NewReaderSize(raw, 4096),
		key:    s.key,
		method: s.method,
		ivLen:  s.ivLen,
	}
	if _, err := c.Write(hdr.Bytes()); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ---- AEAD 模式：分块加密 ----

// ssConn 实现 Shadowsocks AEAD 的流式分块加密：
// 每块 = AEAD(2字节长度) + AEAD(载荷)，各自带 16 字节 tag，nonce 逐块递增。
type ssConn struct {
	net.Conn
	key     []byte
	keyLen  int
	newAEAD func([]byte) (cipher.AEAD, error)

	br *bufio.Reader

	w       cipher.AEAD
	wNonce  []byte
	wSaltOK bool

	r       cipher.AEAD
	rNonce  []byte
	rSaltOK bool

	rbuf []byte
}

func newSSConn(conn net.Conn, key []byte, keyLen int, fn func([]byte) (cipher.AEAD, error)) (*ssConn, error) {
	return &ssConn{
		Conn:    conn,
		key:     key,
		keyLen:  keyLen,
		newAEAD: fn,
		br:      bufio.NewReaderSize(conn, 4096),
	}, nil
}

func (c *ssConn) Write(p []byte) (int, error) {
	if !c.wSaltOK {
		salt := make([]byte, c.keyLen)
		if _, err := rand.Read(salt); err != nil {
			return 0, err
		}
		subkey, err := hkdf.Key(sha1.New, c.key, salt, "ss-subkey", c.keyLen)
		if err != nil {
			return 0, err
		}
		aead, err := c.newAEAD(subkey)
		if err != nil {
			return 0, err
		}
		c.w, c.wNonce, c.wSaltOK = aead, make([]byte, aead.NonceSize()), true
		if _, err := c.Conn.Write(salt); err != nil {
			return 0, err
		}
	}

	const maxChunk = 0x3FFF
	written := 0
	for written < len(p) {
		n := len(p) - written
		if n > maxChunk {
			n = maxChunk
		}
		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], uint16(n))

		sealedLen := c.w.Seal(nil, c.wNonce, lenBuf[:], nil)
		incrementNonce(c.wNonce)
		sealedPayload := c.w.Seal(nil, c.wNonce, p[written:written+n], nil)
		incrementNonce(c.wNonce)

		if _, err := c.Conn.Write(sealedLen); err != nil {
			return written, err
		}
		if _, err := c.Conn.Write(sealedPayload); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (c *ssConn) Read(p []byte) (int, error) {
	for len(c.rbuf) == 0 {
		if err := c.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *ssConn) readChunk() error {
	if !c.rSaltOK {
		salt := make([]byte, c.keyLen)
		if _, err := io.ReadFull(c.br, salt); err != nil {
			return err
		}
		subkey, err := hkdf.Key(sha1.New, c.key, salt, "ss-subkey", c.keyLen)
		if err != nil {
			return err
		}
		aead, err := c.newAEAD(subkey)
		if err != nil {
			return err
		}
		c.r, c.rNonce, c.rSaltOK = aead, make([]byte, aead.NonceSize()), true
	}

	lenBuf := make([]byte, 2+c.r.Overhead())
	if _, err := io.ReadFull(c.br, lenBuf); err != nil {
		return err
	}
	plainLen, err := c.r.Open(lenBuf[:0], c.rNonce, lenBuf, nil)
	if err != nil {
		return fmt.Errorf("ss: decrypt length: %w", err)
	}
	incrementNonce(c.rNonce)

	size := int(binary.BigEndian.Uint16(plainLen))
	payload := make([]byte, size+c.r.Overhead())
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return err
	}
	plain, err := c.r.Open(payload[:0], c.rNonce, payload, nil)
	if err != nil {
		return fmt.Errorf("ss: decrypt payload: %w", err)
	}
	incrementNonce(c.rNonce)
	c.rbuf = plain
	return nil
}

// incrementNonce 按 Shadowsocks 约定做小端递增。
func incrementNonce(b []byte) {
	for i := range b {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

// ---- 流加密模式：CFB / RC4-MD5 ----

// ssStreamConn 实现经典 Shadowsocks 流加密：报文头部带随机 IV，之后整条流即为密文。
type ssStreamConn struct {
	net.Conn
	br     *bufio.Reader
	key    []byte
	method string
	ivLen  int

	reader cipher.Stream
	writer cipher.Stream
}

func newSSStream(method string, key, iv []byte, encrypt bool) (cipher.Stream, error) {
	if method == "rc4-md5" {
		joined := make([]byte, 0, len(key)+len(iv))
		joined = append(joined, key...)
		joined = append(joined, iv...)
		sum := md5.Sum(joined)
		return rc4.NewCipher(sum[:])
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if encrypt {
		return cipher.NewCFBEncrypter(block, iv), nil
	}
	return cipher.NewCFBDecrypter(block, iv), nil
}

func (c *ssStreamConn) Write(p []byte) (int, error) {
	if c.writer == nil {
		iv := make([]byte, c.ivLen)
		if _, err := rand.Read(iv); err != nil {
			return 0, err
		}
		stream, err := newSSStream(c.method, c.key, iv, true)
		if err != nil {
			return 0, err
		}
		c.writer = stream
		if _, err := c.Conn.Write(iv); err != nil {
			return 0, err
		}
	}
	buf := make([]byte, len(p))
	c.writer.XORKeyStream(buf, p)
	return c.Conn.Write(buf)
}

func (c *ssStreamConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		iv := make([]byte, c.ivLen)
		if _, err := io.ReadFull(c.br, iv); err != nil {
			return 0, err
		}
		stream, err := newSSStream(c.method, c.key, iv, false)
		if err != nil {
			return 0, err
		}
		c.reader = stream
	}
	n, err := c.br.Read(p)
	if n > 0 {
		c.reader.XORKeyStream(p[:n], p[:n])
	}
	return n, err
}
