package ws

import (
	"encoding/binary"
	"testing"

	"github.com/gamah/splitclicker/internal/game"
)

func TestEncodeTickLayout(t *testing.T) {
	f := game.TickFrame{
		Round:     3,
		Remaining: 70000, // exceeds uint16 — must round-trip through the u32 field
		Pips: []game.TickPip{
			{Tag: "0a0b0c0d", X: -100, Y: 200, TArmMs: 1234},
		},
	}
	b := encodeTick(f)
	if len(b) != tickHeaderLen+tickPipLen {
		t.Fatalf("len=%d want %d", len(b), tickHeaderLen+tickPipLen)
	}
	if b[0] != tickOpcode {
		t.Fatalf("opcode=%d want %d", b[0], tickOpcode)
	}
	if got := binary.LittleEndian.Uint16(b[1:]); got != 3 {
		t.Errorf("round=%d want 3", got)
	}
	if got := binary.LittleEndian.Uint32(b[3:]); got != 70000 {
		t.Errorf("remaining=%d want 70000", got)
	}
	if b[7] != 1 {
		t.Fatalf("count=%d want 1", b[7])
	}
	off := tickHeaderLen
	if got := binary.LittleEndian.Uint32(b[off:]); got != 0x0a0b0c0d {
		t.Errorf("tag=%08x want 0a0b0c0d", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b[off+4:])); got != -100 {
		t.Errorf("x=%d want -100", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b[off+6:])); got != 200 {
		t.Errorf("y=%d want 200", got)
	}
	if got := binary.LittleEndian.Uint16(b[off+8:]); got != 1234 {
		t.Errorf("t_arm=%d want 1234", got)
	}
}

func TestEncodeTickNegativeRemainingClampsZero(t *testing.T) {
	b := encodeTick(game.TickFrame{Round: 1, Remaining: -3})
	if got := binary.LittleEndian.Uint32(b[3:]); got != 0 {
		t.Errorf("remaining=%d want 0 (clamped)", got)
	}
}

func TestParseTag(t *testing.T) {
	// Server PlayerTag is lower-case 8-hex, including leading zeros — all must parse.
	cases := map[string]uint32{
		"00000000": 0,
		"00000001": 1,
		"0a0b0c0d": 0x0a0b0c0d,
		"deadbeef": 0xdeadbeef,
	}
	for tag, want := range cases {
		if got := parseTag(tag); got != want {
			t.Errorf("parseTag(%q)=%08x want %08x", tag, got, want)
		}
	}
}
