package usage

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func GenerateTraceID() string {
	return randomNonZeroHex(16)
}

func GenerateSpanID() string {
	return randomNonZeroHex(8)
}

// ValidTraceID reports whether v is a W3C trace id: 32 lowercase hex characters,
// not all zeroes. Uppercase is rejected rather than normalized, because a caller
// that sends it is not speaking the format the gateway echoes back.
func ValidTraceID(v string) bool {
	return validLowerHexNonZero(v, 32)
}

// ValidSpanID reports whether v is a W3C span id: 16 lowercase hex characters,
// not all zeroes. It rejects uppercase for the same reason as [ValidTraceID].
func ValidSpanID(v string) bool {
	return validLowerHexNonZero(v, 16)
}

// ValidLowerHex reports whether v is exactly n lowercase hex characters. Unlike
// ValidTraceID and ValidSpanID it permits the all-zero form, which is legal for
// traceparent fields that are not ids.
func ValidLowerHex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, ch := range v {
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' {
			continue
		}
		return false
	}
	return true
}

// randomNonZeroHex returns size random bytes as lowercase hex, never the
// all-zero form that W3C Trace Context reserves as invalid. It panics if the
// system entropy source fails: crypto/rand.Read is documented never to return
// an error, and a trace id generator that degrades to a constant on failure
// would silently collapse every request onto one trace.
func randomNonZeroHex(size int) string {
	buf := make([]byte, size)
	for {
		if _, err := rand.Read(buf); err != nil {
			panic("usage: crypto/rand read failed: " + err.Error())
		}
		if !allZero(buf) {
			return hex.EncodeToString(buf)
		}
	}
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func validLowerHexNonZero(v string, n int) bool {
	return ValidLowerHex(v, n) && strings.Trim(v, "0") != ""
}
