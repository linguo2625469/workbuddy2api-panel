// Package util — tiny shared helpers (uuid, token log truncation).
package util

import "crypto/rand"

// GenerateUUID — UUIDv4 (hex-encoded).
func GenerateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	const hexLower = "0123456789abcdef"
	var dst [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst[j] = '-'
			j++
		}
		dst[j] = hexLower[b[i]>>4]
		dst[j+1] = hexLower[b[i]&0xF]
		j += 2
	}
	return string(dst[:])
}

// TruncateToken returns first 10 + ... + last 10 chars for log readability.
func TruncateToken(s string) string {
	if len(s) <= 23 {
		return s
	}
	return s[:10] + "..." + s[len(s)-10:]
}
