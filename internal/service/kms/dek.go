package kms

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// String implements fmt.Stringer without the plaintext key: the key id and
// the size of the sealed key identify a DEK in logs and errors.
func (d DEK) String() string {
	return "DEK{KeyID:" + d.KeyID + " Ciphertext:" + strconv.Itoa(len(d.Ciphertext)) + " bytes}"
}

// GoString implements fmt.GoStringer (%#v) without the plaintext key.
func (d DEK) GoString() string { return d.String() }

// Format implements fmt.Formatter: every verb (%v, %+v, %#v, %x...) prints
// the redacted form, never the 256-bit key.
func (d DEK) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, d.String()) }

// LogValue implements slog.LogValuer without the plaintext key.
func (d DEK) LogValue() slog.Value {
	return slog.GroupValue(slog.String("keyID", d.KeyID), slog.Int("ciphertextLength", len(d.Ciphertext)))
}

// MarshalJSON keeps the plaintext key out of any JSON form (a log record,
// an audit entry): only the key id and the size of the sealed key.
func (d DEK) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		KeyID            string `json:"keyID"`
		CiphertextLength int    `json:"ciphertextLength"`
	}{d.KeyID, len(d.Ciphertext)})
}
