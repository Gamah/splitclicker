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
//	u32 remaining           clicks-remaining this round (exact; can exceed 65535)
//	u8  count               number of pips that follow (≤ TickSampleK, ≤ 255)
//	count × pip:
//	  u32 tag               the player's public Tag (8 hex chars ⇒ 32 bits)
//	  i16 x, i16 y          normalized click position (−32767..32767, 0 = centre)
//	  u16 t_arm             ms since the round armed (jitter-buffer replay offset)
//
// The leading opcode lets the client distinguish frame kinds if more binary
// frames are ever added; today any binary message is a tick.
const tickOpcode byte = 1

const tickHeaderLen = 1 + 2 + 4 + 1 // opcode + round + remaining + count
const tickPipLen = 4 + 2 + 2 + 2    // tag + x + y + t_arm

// encodeTick packs a TickFrame into the binary wire form above. Pips beyond 255
// are dropped (the sampler already caps at TickSampleK, well under that).
func encodeTick(f game.TickFrame) []byte {
	n := len(f.Pips)
	if n > 255 {
		n = 255
	}
	buf := make([]byte, tickHeaderLen+n*tickPipLen)
	buf[0] = tickOpcode
	binary.LittleEndian.PutUint16(buf[1:], uint16(f.Round))
	rem := f.Remaining
	if rem < 0 {
		rem = 0
	}
	binary.LittleEndian.PutUint32(buf[3:], uint32(rem))
	buf[7] = byte(n)
	off := tickHeaderLen
	for i := 0; i < n; i++ {
		p := f.Pips[i]
		binary.LittleEndian.PutUint32(buf[off:], parseTag(p.Tag))
		binary.LittleEndian.PutUint16(buf[off+4:], uint16(p.X)) // int16 → two's-complement bits
		binary.LittleEndian.PutUint16(buf[off+6:], uint16(p.Y))
		binary.LittleEndian.PutUint16(buf[off+8:], p.TArmMs)
		off += tickPipLen
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
