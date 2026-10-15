//go:build webssh

package webssh

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeFrame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{"data empty", msgData, nil},
		{"data bytes", msgData, []byte("hello")},
		{"resize json", msgResize, []byte(`{"cols":80,"rows":24}`)},
		{"file chunk", msgFileUploadData, bytes.Repeat([]byte{0xAB}, 32768)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := encodeFrame(tc.typ, tc.payload)
			typ, payload, err := decodeFrame(frame)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if typ != tc.typ {
				t.Fatalf("type = %#x, want %#x", typ, tc.typ)
			}
			if !bytes.Equal(payload, tc.payload) {
				t.Fatalf("payload mismatch: %d bytes, want %d", len(payload), len(tc.payload))
			}
		})
	}
}

func TestDecodeFrameInvalid(t *testing.T) {
	if _, _, err := decodeFrame(nil); err == nil {
		t.Fatal("empty frame must error")
	}
	if _, _, err := decodeFrame([]byte{0x00}); err == nil {
		t.Fatal("unknown type 0x00 must error")
	}
	if _, _, err := decodeFrame([]byte{0x42, 'x'}); err == nil {
		t.Fatal("unknown type 0x42 must error")
	}
	if _, _, err := decodeFrame([]byte{msgData}); err != nil {
		t.Fatalf("type-only frame should be valid: %v", err)
	}
}
