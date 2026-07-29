package solana

import (
	"fmt"
	"strings"
)

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var decodeMap = func() [256]int {
	var m [256]int
	for i := range m {
		m[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		m[alphabet[i]] = i
	}
	return m
}()

// DecodeBase58 decodes a canonical Bitcoin/Solana base58 string. It rejects
// malformed input and non-canonical encodings.
func DecodeBase58(s string, maxDecodedLen int) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("base58 value is required")
	}
	if strings.TrimSpace(s) != s {
		return nil, fmt.Errorf("base58 value contains surrounding whitespace")
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}

	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		val := decodeMap[s[i]]
		if val < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", s[i])
		}
		carry := val
		for j := 0; j < len(out); j++ {
			carry += int(out[j]) * 58
			out[j] = byte(carry & 0xff)
			carry >>= 8
		}
		for carry > 0 {
			out = append(out, byte(carry&0xff))
			carry >>= 8
			if maxDecodedLen > 0 && len(out)+zeros > maxDecodedLen {
				return nil, fmt.Errorf("decoded value too long")
			}
		}
	}

	decoded := make([]byte, zeros+len(out))
	for i := 0; i < zeros; i++ {
		decoded[i] = 0
	}
	for i := 0; i < len(out); i++ {
		decoded[len(decoded)-1-i] = out[i]
	}
	if maxDecodedLen > 0 && len(decoded) > maxDecodedLen {
		return nil, fmt.Errorf("decoded value too long")
	}
	if EncodeBase58(decoded) != s {
		return nil, fmt.Errorf("non-canonical base58 encoding")
	}
	return decoded, nil
}

// EncodeBase58 emits the canonical base58 encoding of b.
func EncodeBase58(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}

	digits := make([]byte, 0, len(b)*138/100+1)
	for i := zeros; i < len(b); i++ {
		carry := int(b[i])
		for j := 0; j < len(digits); j++ {
			carry += int(digits[j]) << 8
			digits[j] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append(digits, byte(carry%58))
			carry /= 58
		}
	}

	var sb strings.Builder
	sb.Grow(zeros + len(digits))
	for i := 0; i < zeros; i++ {
		sb.WriteByte('1')
	}
	for i := len(digits) - 1; i >= 0; i-- {
		sb.WriteByte(alphabet[digits[i]])
	}
	return sb.String()
}
