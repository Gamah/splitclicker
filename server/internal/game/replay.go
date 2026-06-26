package game

// Durable game replay — the data the admin replay viewer plays back. Everything is
// keyed to milliseconds from the round's arm (the same clock the scoring clicks and
// board claims already use), so cursors, button spawns, and claims all sit on one
// timeline. Captured in memory across a game and flushed with the game history in a
// single transaction (a replay that fails to write rolls the whole game back — see
// store.RecordGame), so "no replay ⇒ no round" holds.
//
// Cursor data exists only for the armed window: the client reports its pointer just
// while it is on the board, so a replay shows bursts of motion during each window
// with gaps over the (silent) arm delays in between. That is inherent to the live
// data and accepted — no continuous-capture client change was made.

// CursorSample is one recorded pointer position: ms offset from the round's arm and
// the normalized position (int16, 0 = centre, matching the click/button coordinate
// space).
type CursorSample struct {
	TMs int   `json:"t"`
	X   int16 `json:"x"`
	Y   int16 `json:"y"`
}

// CursorTrack is one player's recorded pointer path for an armed window plus the
// display info the viewer needs to label the dot. Supplied by the ws hub
// (SetCursorTracksFn), keyed by SteamID.
type CursorTrack struct {
	Tag      string
	Username string
	Samples  []CursorSample
}

// ReplayPlayer is one player present in a round's replay: their public tag (the key
// claims/cursors reference) and display name. The viewer assigns each a colour.
type ReplayPlayer struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
}

// ReplayButton is one live button shown during the window: its slot id, normalized
// position, and the ms offset from arm at which it appeared (0 for the initial board;
// for a replacement, the claim time of the button it replaced).
type ReplayButton struct {
	ID  uint16 `json:"id"`
	X   int16  `json:"x"`
	Y   int16  `json:"y"`
	TMs int    `json:"t"`
}

// ReplayClaim is one button claim: the slot taken, the claimer's tag, and the ms
// offset from arm it happened.
type ReplayClaim struct {
	Slot uint16 `json:"slot"`
	By   string `json:"by"`
	TMs  int    `json:"t"`
}

// ReplayCursor is one player's cursor path in the served replay: the tag it belongs
// to and the ordered samples.
type ReplayCursor struct {
	Tag     string         `json:"tag"`
	Samples []CursorSample `json:"s"`
}

// RoundReplay is one round's full visualization payload.
type RoundReplay struct {
	No      int            `json:"no"`
	N       int            `json:"n"`
	DurMs   int            `json:"dur_ms"`
	Players []ReplayPlayer `json:"players"`
	Buttons []ReplayButton `json:"buttons"`
	Claims  []ReplayClaim  `json:"claims"`
	Cursors []ReplayCursor `json:"cursors"`
}

// GameReplay is the whole game's replay (every round), serialized + gzipped and
// stored as one row alongside the game history.
type GameReplay struct {
	GameID string        `json:"game_id"`
	Rounds []RoundReplay `json:"rounds"`
}

// buildRoundReplay assembles one round's replay from the board (its initial buttons +
// full claim log) and the hub's per-window cursor paths. The player roster (tag→name)
// is the union of everyone who moved a cursor and everyone who claimed a button, so a
// wire-bot that claimed without ever moving onto the board still gets a labelled entry
// (name falls back to its tag). Called at the end of race, before the next arming
// stage clears the hub's per-window capture.
func (e *Engine) buildRoundReplay(round, n, durMs int, initial []Button, b *board) RoundReplay {
	tracks := e.cursorTracks()

	// tag → display name, preferring the cursor track's name (it's the live roster).
	names := make(map[string]string, len(tracks))
	for _, t := range tracks {
		if t.Tag != "" {
			names[t.Tag] = t.Username
		}
	}
	for _, ev := range b.scored {
		if ev.Tag == "" {
			continue
		}
		if _, ok := names[ev.Tag]; !ok {
			names[ev.Tag] = ev.Username
		}
	}

	players := make([]ReplayPlayer, 0, len(names))
	for tag, name := range names {
		if name == "" {
			name = tag
		}
		players = append(players, ReplayPlayer{Tag: tag, Name: name})
	}

	buttons := make([]ReplayButton, 0, len(initial)+len(b.log))
	for _, btn := range initial {
		buttons = append(buttons, ReplayButton{ID: btn.SlotID, X: btn.X, Y: btn.Y, TMs: 0})
	}
	claims := make([]ReplayClaim, 0, len(b.log))
	for _, cl := range b.log {
		claims = append(claims, ReplayClaim{Slot: cl.SlotID, By: cl.ClaimerTag, TMs: int(cl.TArmMs)})
		if cl.Spawn != nil {
			buttons = append(buttons, ReplayButton{ID: cl.Spawn.SlotID, X: cl.Spawn.X, Y: cl.Spawn.Y, TMs: int(cl.TArmMs)})
		}
	}

	cursors := make([]ReplayCursor, 0, len(tracks))
	for _, t := range tracks {
		if len(t.Samples) == 0 {
			continue
		}
		cursors = append(cursors, ReplayCursor{Tag: t.Tag, Samples: t.Samples})
	}

	return RoundReplay{
		No: round, N: n, DurMs: durMs,
		Players: players, Buttons: buttons, Claims: claims, Cursors: cursors,
	}
}
