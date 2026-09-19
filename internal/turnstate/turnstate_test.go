package turnstate

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

func TestFernetIssuedAt(t *testing.T) {
	t.Parallel()
	issuedAt := time.Date(2026, 9, 19, 12, 34, 56, 0, time.UTC)
	raw := make([]byte, fernetPrefixBytes+fernetIVBytes+10*fernetBlockBytes+fernetHMACBytes)
	raw[0] = fernetVersion
	binary.BigEndian.PutUint64(raw[1:fernetPrefixBytes], uint64(issuedAt.Unix()))
	value := base64.URLEncoding.EncodeToString(raw)
	if len(value) != 292 {
		t.Fatalf("fixture length = %d, want 292", len(value))
	}

	for _, candidate := range []string{value, value[:len(value)-2]} {
		got, ok := FernetIssuedAt(candidate)
		if !ok || !got.Equal(issuedAt) {
			t.Fatalf("FernetIssuedAt() = %s, %v; want %s, true", got, ok, issuedAt)
		}
	}
}

func TestFernetIssuedAtRejectsInvalidFraming(t *testing.T) {
	t.Parallel()
	tests := []string{
		"",
		"not-a-token",
		"AAAA====",
		base64.URLEncoding.EncodeToString(make([]byte, 73)),
	}
	for _, value := range tests {
		if got, ok := FernetIssuedAt(value); ok || !got.IsZero() {
			t.Fatalf("FernetIssuedAt(%q) = %s, %v; want zero, false", value, got, ok)
		}
	}
}
