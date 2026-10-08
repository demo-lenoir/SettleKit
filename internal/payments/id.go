package payments

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidID = errors.New("invalid intent UUID")

func NewID() ([16]byte, string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return raw, "", fmt.Errorf("generate intent ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return raw, FormatID(raw), nil
}

func FormatID(raw [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func ParseID(s string) ([16]byte, error) {
	var raw [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' || s != strings.ToLower(s) {
		return raw, ErrInvalidID
	}
	compact := strings.ReplaceAll(s, "-", "")
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 {
		return raw, ErrInvalidID
	}
	copy(raw[:], decoded)
	if FormatID(raw) != s || raw[6]>>4 != 4 || raw[8]>>6 != 2 {
		return [16]byte{}, ErrInvalidID
	}
	return raw, nil
}
