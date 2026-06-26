package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/gamah/splitclicker/internal/game"
	"github.com/jackc/pgx/v5"
)

// buildReplayBlob assembles a game's replay (every round's buttons/claims/cursor
// paths) from its GameLog and returns it as gzipped JSON — the payload stored in
// game_replays. Returns nil when the game has no replay rounds (nothing to store).
func buildReplayBlob(log game.GameLog) ([]byte, error) {
	gr := game.GameReplay{GameID: log.GameID, Rounds: make([]game.RoundReplay, 0, len(log.RoundLogs))}
	for _, r := range log.RoundLogs {
		gr.Rounds = append(gr.Rounds, r.Replay)
	}
	if len(gr.Rounds) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(gr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// GameReplayJSON loads a game's stored replay and returns it as decompressed JSON
// (ready to hand straight to the viewer). ok is false when the game has no replay.
func (s *Store) GameReplayJSON(ctx context.Context, gameID string) (raw []byte, ok bool, err error) {
	var blob []byte
	err = s.pool.QueryRow(ctx,
		`SELECT data FROM game_replays WHERE game_id = $1`, gameID,
	).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, false, err
	}
	defer zr.Close()
	raw, err = io.ReadAll(zr)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}
