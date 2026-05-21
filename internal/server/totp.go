package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

func totpGenerateSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return strings.TrimRight(base32.StdEncoding.EncodeToString(b), "=")
}

func totpCode(secret string, t int64) string {
	s := strings.ToUpper(strings.TrimSpace(secret))
	pad := len(s) % 8
	if pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	key, err := base32.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	counter := uint64(t / 30)
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg)
	h := mac.Sum(nil)
	offset := h[len(h)-1] & 0x0f
	code := int(h[offset]&0x7f)<<24 |
		int(h[offset+1])<<16 |
		int(h[offset+2])<<8 |
		int(h[offset+3])
	return fmt.Sprintf("%06d", code%1000000)
}

func totpVerify(secret, code string) bool {
	now := time.Now().Unix()
	for delta := int64(-1); delta <= 1; delta++ {
		if totpCode(secret, now+delta*30) == code {
			return true
		}
	}
	return false
}
