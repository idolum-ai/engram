//go:build darwin

package app

import (
	"encoding/binary"
	"testing"
)

func TestDarwinBootIDUsesExactBootTime(t *testing.T) {
	value := make([]byte, 16)
	binary.NativeEndian.PutUint64(value[:8], uint64(1_753_000_001))
	binary.NativeEndian.PutUint32(value[8:12], uint32(42))
	if got := darwinBootID(value); got != "darwin:1753000001.000042" {
		t.Fatalf("darwinBootID() = %q", got)
	}
	for _, invalid := range [][]byte{
		nil,
		make([]byte, 16),
		func() []byte {
			out := append([]byte(nil), value...)
			binary.NativeEndian.PutUint32(out[8:12], uint32(1_000_000))
			return out
		}(),
	} {
		if got := darwinBootID(invalid); got != "" {
			t.Fatalf("darwinBootID(%v) = %q, want empty", invalid, got)
		}
	}
}
