package ws

import (
	"encoding/binary"
	"strconv"

	"github.com/gamah/splitclicker/internal/game"
)

// The live-window `tick` frame is the one binary frame on the wire (CLAUDE.md
// hot-path note): it fires up to ~20×/s while the button is armed, so it skips
// JSON. Everything else stays JSON text. Layout (little-endian):
//
//	u8  opcode (tickOpcode)
//	u16 round
//	u32 remaining               clicks-remaining this round (exact; can exceed 65535)
//	u16 claimCount              board mutations since the last tick (authoritative, complete)
//	claimCount × claim:
//	  u16 slot_id               the claimed button's slot id
//	  u32 claimer_tag           claimer's public Tag (8 hex chars ⇒ 32 bits)
//	  u16 t_arm                 ms since arm (jitter-buffer pip replay offset)
//	  u8  spawned               1 ⇒ a replacement button follows, 0 ⇒ none (round ended)
//	  if spawned:
//	    u16 new_slot_id
//	    u64 new_nonce           the replacement's secret (a scoring click must echo it)
//	    i16 new_x, i16 new_y    the replacement's normalized position (0 = centre)
//	u8  cursorCount             sampled opponent cursors (≤ CursorSampleK, ≤ 255)
//	cursorCount × cursor:
//	  u32 tag                   the cursor owner's public Tag
//	  i16 x, i16 y              normalized cursor position (0 = centre)
//
// Claims are complete (never sampled) because a dropped one would leave a client
// showing a dead button or missing a live one; cursors are cosmetic, so capped.
// The leading opcode lets the client distinguish frame kinds if more binary frames
// are ever added; today any binary message is a tick.
const tickOpcode byte = 1

// cursorSample is one opponent cursor the hub samples into a tick (its position lives
// in the hub's per-connection state, not in the engine's TickFrame).
type cursorSample struct {
	Tag  string
	X, Y int16
}

// encodeTick packs a TickFrame plus the hub's sampled cursors into the binary wire
// form above. claimCount caps at 65535 and cursorCount at 255 (both far above any
// real tick: claims are bounded by scoring throughput, cursors by CursorSampleK).
func encodeTick(f game.TickFrame, cursors []cursorSample) []byte {
	nc := len(f.Claims)
	if nc > 65535 {
		nc = 65535
	}
	ncur := len(cursors)
	if ncur > 255 {
		ncur = 255
	}
	buf := make([]byte, 0, 9+nc*23+1+ncur*8)
	buf = append(buf, tickOpcode)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(f.Round))
	rem := f.Remaining
	if rem < 0 {
		rem = 0
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rem))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(nc))
	for i := 0; i < nc; i++ {
		c := f.Claims[i]
		buf = binary.LittleEndian.AppendUint16(buf, c.SlotID)
		buf = binary.LittleEndian.AppendUint32(buf, parseTag(c.ClaimerTag))
		buf = binary.LittleEndian.AppendUint16(buf, c.TArmMs)
		if c.Spawn != nil {
			buf = append(buf, 1)
			buf = binary.LittleEndian.AppendUint16(buf, c.Spawn.SlotID)
			buf = binary.LittleEndian.AppendUint64(buf, c.Spawn.Nonce)
			buf = binary.LittleEndian.AppendUint16(buf, uint16(c.Spawn.X)) // int16 → two's-complement bits
			buf = binary.LittleEndian.AppendUint16(buf, uint16(c.Spawn.Y))
		} else {
			buf = append(buf, 0)
		}
	}
	buf = append(buf, byte(ncur))
	for i := 0; i < ncur; i++ {
		cur := cursors[i]
		buf = binary.LittleEndian.AppendUint32(buf, parseTag(cur.Tag))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(cur.X))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(cur.Y))
	}
	return buf
}

// parseTag turns the 8-hex-char public Tag (session.PlayerTag — the first 8 hex
// chars of a sha256) into the uint32 the binary frame carries. The client formats
// it back with the same %08x to key the roster, so leading zeros round-trip.
func parseTag(tag string) uint32 {
	v, _ := strconv.ParseUint(tag, 16, 32)
	return uint32(v)
}
