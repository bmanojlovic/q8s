package server

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestReadFrameRejectsOversizedLength pins the frame cap: a client
// advertising a huge 64-bit length must get an error, not a huge allocation.
func TestReadFrameRejectsOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x82) // FIN + binary
	buf.WriteByte(0xFF) // masked + 7-bit length code 127 (64-bit length)
	var ext [8]byte
	binary.BigEndian.PutUint64(ext[:], 1<<40) // 1 TiB
	buf.Write(ext[:])
	buf.Write([]byte{1, 2, 3, 4}) // mask key

	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("expected error for oversized frame length")
	} else if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReadFrameAcceptsSmallFrame: the cap must not break normal frames.
func TestReadFrameAcceptsSmallFrame(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{chanStdin, 'h', 'i'}
	buf.WriteByte(0x82)                      // FIN + binary
	buf.WriteByte(byte(0x80 | len(payload))) // masked
	buf.Write([]byte{0, 0, 0, 0})            // zero mask = identity
	buf.Write(payload)

	op, got, err := readFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if op != wsOpBinary || !bytes.Equal(got, payload) {
		t.Errorf("got op=%d payload=%q", op, got)
	}
}
