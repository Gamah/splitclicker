package ws

import (
	"encoding/binary"
	"testing"

	"github.com/gamah/splitclicker/internal/game"
)

func TestEncodeTickLayout(t *testing.T) {
	spawn := game.Button{SlotID: 77, Nonce: 0x1122334455667788, X: -100, Y: 200}
	f := game.TickFrame{
		Round:     3,
		Remaining: 70000, // exceeds uint16 — must round-trip through the u32 field
		Claims: []game.BoardClaim{
			{SlotID: 12, ClaimerTag: "0a0b0c0d", TArmMs: 1234, Spawn: &spawn},
		},
	}
	cursors := []cursorSample{{Tag: "deadbeef", X: 5, Y: -6}}
	b := encodeTick(f, cursors)

	if b[0] != tickOpcode {
		t.Fatalf("opcode=%d want %d", b[0], tickOpcode)
	}
	if got := binary.LittleEndian.Uint16(b[1:]); got != 3 {
		t.Errorf("round=%d want 3", got)
	}
	if got := binary.LittleEndian.Uint32(b[3:]); got != 70000 {
		t.Errorf("remaining=%d want 70000", got)
	}
	if got := binary.LittleEndian.Uint16(b[7:]); got != 1 {
		t.Fatalf("claimCount=%d want 1", got)
	}
	off := 9 // opcode + round + remaining + claimCount
	if got := binary.LittleEndian.Uint16(b[off:]); got != 12 {
		t.Errorf("slot=%d want 12", got)
	}
	if got := binary.LittleEndian.Uint32(b[off+2:]); got != 0x0a0b0c0d {
		t.Errorf("claimer tag=%08x want 0a0b0c0d", got)
	}
	if got := binary.LittleEndian.Uint16(b[off+6:]); got != 1234 {
		t.Errorf("t_arm=%d want 1234", got)
	}
	if b[off+8] != 1 {
		t.Fatalf("spawned=%d want 1", b[off+8])
	}
	if got := binary.LittleEndian.Uint16(b[off+9:]); got != 77 {
		t.Errorf("new_slot=%d want 77", got)
	}
	if got := binary.LittleEndian.Uint64(b[off+11:]); got != 0x1122334455667788 {
		t.Errorf("new_nonce=%016x want 1122334455667788", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b[off+19:])); got != -100 {
		t.Errorf("new_x=%d want -100", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b[off+21:])); got != 200 {
		t.Errorf("new_y=%d want 200", got)
	}
	// Cursor section follows the (spawned) claim: claim base 9 + spawn 14 = 23 bytes.
	curOff := off + 23
	if b[curOff] != 1 {
		t.Fatalf("cursorCount=%d want 1", b[curOff])
	}
	if got := binary.LittleEndian.Uint32(b[curOff+1:]); got != 0xdeadbeef {
		t.Errorf("cursor tag=%08x want deadbeef", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b[curOff+5:])); got != 5 {
		t.Errorf("cursor x=%d want 5", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b[curOff+7:])); got != -6 {
		t.Errorf("cursor y=%d want -6", got)
	}
	if len(b) != curOff+1+8 {
		t.Errorf("len=%d want %d", len(b), curOff+1+8)
	}
}

// A claim that ends the round carries spawned=0 and no replacement bytes, so the
// cursor count immediately follows the spawned flag.
func TestEncodeTickNoSpawn(t *testing.T) {
	f := game.TickFrame{Round: 1, Remaining: 0, Claims: []game.BoardClaim{
		{SlotID: 9, ClaimerTag: "00000001", TArmMs: 50},
	}}
	b := encodeTick(f, nil)
	off := 9
	if b[off+8] != 0 {
		t.Fatalf("spawned=%d want 0", b[off+8])
	}
	if b[off+9] != 0 {
		t.Fatalf("cursorCount=%d want 0", b[off+9])
	}
	if len(b) != off+9+1 {
		t.Errorf("len=%d want %d", len(b), off+9+1)
	}
}

func TestEncodeTickNegativeRemainingClampsZero(t *testing.T) {
	b := encodeTick(game.TickFrame{Round: 1, Remaining: -3}, nil)
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
