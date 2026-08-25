// Package id generates standard UUID identifiers for marketplace records.
//
// New returns an RFC 9562 UUIDv7 rendered in the canonical 36-character
// lowercase form: the leading 48 bits carry the millisecond timestamp, so
// values still sort lexicographically by creation time (the property the
// schema comments rely on), and the remaining bits are random.
//
// Implemented against the standard library only (crypto/rand), matching the
// repository convention of not adding a dependency until one is required.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// New returns a fresh canonical-form UUIDv7 string.
func New() string {
	return newAt(time.Now())
}

func newAt(t time.Time) string {
	var raw [16]byte

	ms := uint64(t.UnixMilli())
	// Timestamp occupies the high 48 bits (6 bytes).
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)

	// The remaining 10 bytes are randomness (version and variant bits are
	// overwritten below). crypto/rand.Read never returns an error on supported
	// platforms; panic rather than degrade to a time-derived filler, which would
	// make every ID minted in the same millisecond identical and collide.
	if _, err := rand.Read(raw[6:]); err != nil {
		panic("id: crypto/rand unavailable: " + err.Error())
	}

	raw[6] = (raw[6] & 0x0f) | 0x70 // version 7
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 9562 variant

	return Format(raw)
}

// Format renders 16 raw bytes in the canonical 8-4-4-4-12 lowercase UUID form.
// It performs no version or variant fixup; callers own those bits.
func Format(raw [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], raw[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], raw[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], raw[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], raw[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], raw[10:16])
	return string(buf[:])
}
