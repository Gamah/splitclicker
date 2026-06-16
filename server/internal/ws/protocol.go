package ws

import "github.com/gamah/splitclicker/internal/game"

// Server→client frame shapes (JSON text). The hot-path frames are `armed` (out)
// and `click` (in); everything else is incidental. See PLAN.md §8.
//
// nonce is sent as a hex string, not a JSON number: it is an unguessable 64-bit
// token and JSON numbers can't carry full uint64 precision in every client.

type helloYou struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
}

type helloGame struct {
	Round   int    `json:"round"`
	Of      int    `json:"of"`
	Phase   string `json:"phase"`
	Players int    `json:"players"`
	Clicks  int    `json:"clicks"`
	ArmMin  int    `json:"arm_min"` // arming-window bounds, seconds (the delay itself is secret)
	ArmMax  int    `json:"arm_max"`
	// Bad-click penalty escalation (ms), so the client mirrors the live throttle
	// estimate without hardcoding the formula.
	PenaltyBase int `json:"penalty_base_ms"`
	PenaltyStep int `json:"penalty_step_ms"`
	// DevNote is the current host-editable broadcast note (empty = none), so a
	// mid-game joiner shows it without waiting for the next game's dev_note frame.
	DevNote string `json:"dev_note"`
}

// devNoteWire pushes the host-editable broadcast note. An empty note clears it
// on the client. Sent once per game (and whenever it changes between games).
type devNoteWire struct {
	T    string `json:"t"`
	Note string `json:"note"`
}

type helloWire struct {
	T    string    `json:"t"`
	You  helloYou  `json:"you"`
	Game helloGame `json:"game"`
}

type pendingWire struct {
	T       string `json:"t"`
	Round   int    `json:"round"`
	Of      int    `json:"of"`
	Players int    `json:"players"`
	Clicks  int    `json:"clicks"`
}

// penalty_ms is this connection's own arm-delay penalty (the spam deterrent),
// surfaced so a masher can see they're being throttled. 0 for honest clients.
type armedWire struct {
	T         string `json:"t"`
	Round     int    `json:"round"`
	Seq       int    `json:"seq"`
	Nonce     string `json:"nonce"`
	Players   int    `json:"players"`
	Clicks    int    `json:"clicks"`
	PenaltyMs int    `json:"penalty_ms"`
}

// youResult lets the client drive its `points` achievement stat exactly once:
// apply points_delta only for an unseen round_id (§7.1).
type youResult struct {
	PointsDelta int    `json:"points_delta"`
	RoundID     string `json:"round_id"`
}

type resultWire struct {
	T         string          `json:"t"`
	Round     int             `json:"round"`
	Of        int             `json:"of"`
	Winners   []game.Standing `json:"winners"`
	Standings []game.Standing `json:"standings"`
	You       youResult       `json:"you"`
}

// youGameOver drives placement/win achievements; game_id dedupes (§7.1).
// points_delta/round_id carry the FINAL round's score (the last round has no
// round_result of its own) so the client can drive its `points` stat once.
type youGameOver struct {
	Placement   int    `json:"placement"`
	Won         bool   `json:"won"`
	GameID      string `json:"game_id"`
	PointsDelta int    `json:"points_delta"`
	RoundID     string `json:"round_id"`
}

type gameOverWire struct {
	T         string          `json:"t"`
	Standings []game.Standing `json:"standings"`
	You       youGameOver     `json:"you"`
}
