package lalamove

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// rawSignature is the documented string to sign:
// TIMESTAMP \r\n METHOD \r\n PATH \r\n\r\n BODY.
func rawSignature(timestamp, method, path string, body []byte) string {
	var b strings.Builder
	b.Grow(len(timestamp) + len(method) + len(path) + len(body) + 8)
	b.WriteString(timestamp)
	b.WriteString("\r\n")
	b.WriteString(method)
	b.WriteString("\r\n")
	b.WriteString(path)
	b.WriteString("\r\n\r\n")
	b.Write(body)
	return b.String()
}

// sign returns the lowercase hex HMAC-SHA256 of the raw signature string.
func sign(secret, timestamp, method, path string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(rawSignature(timestamp, method, path, body)))
	return hex.EncodeToString(m.Sum(nil))
}

// newRequestID returns a random RFC 4122 version-4 UUID from crypto/rand.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:]), nil
}
