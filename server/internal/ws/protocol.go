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
	Round int    `json:"round"`
	Of    int    `json:"of"`
	Phase string `json:"phase"`
}

type helloWire struct {
	T    string    `json:"t"`
	You  helloYou  `json:"you"`
	Game helloGame `json:"game"`
}

type pendingWire struct {
	T     string `json:"t"`
	Round int    `json:"round"`
	Of    int    `json:"of"`
}

type armedWire struct {
	T     string `json:"t"`
	Round int    `json:"round"`
	Seq   int    `json:"seq"`
	Nonce string `json:"nonce"`
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
type youGameOver struct {
	Placement int    `json:"placement"`
	Won       bool   `json:"won"`
	GameID    string `json:"game_id"`
}

type gameOverWire struct {
	T         string          `json:"t"`
	Standings []game.Standing `json:"standings"`
	You       youGameOver     `json:"you"`
}
