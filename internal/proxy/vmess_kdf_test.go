package proxy

import (
	"encoding/hex"
	"testing"
)

// TestVMessKDFVector 用 Xray 官方实现产生的向量锁定 KDF 结果。
// 向量来源：github.com/xtls/xray-core/proxy/vmess/aead KDF(key, "AES Auth ID Encryption")。
func TestVMessKDFVector(t *testing.T) {
	key := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	const want = "d36dc1757471f558494c042fdcab9eae8ec58065f2502579e552760178815412"

	got := hex.EncodeToString(vmessKDF(key, "AES Auth ID Encryption"))
	if got != want {
		t.Fatalf("KDF 结果不匹配\n got=%s\nwant=%s", got, want)
	}
}
