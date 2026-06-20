package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Bounty is one skin offered for a timeframe, with its (eventual) winner. See
// migrations/00008_bounties.sql for the lifecycle (pending → active → won).
type Bounty struct {
	ID          int64
	SkinImage   string
	Label       string
	WinTime     time.Time
	Status      string // pending | active | won
	ActivatedAt *time.Time
	WinnerID    string // "" until finalized (or empty window)
	WinnerName  string
	WinnerWins  int
	WonAt       *time.Time
	CreatedAt   time.Time
}

// ActiveBounty returns the currently active bounty (the skin shown + the
// countdown). ok is false when no bounty is active (e.g. before any is added or
// after the queue has drained) — callers fall back to config.json/env.
func (s *Store) ActiveBounty(ctx context.Context) (b Bounty, ok bool, err error) {
	err = s.scanBounty(s.pool.QueryRow(ctx, bountySelect+` WHERE status = 'active' LIMIT 1`), &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bounty{}, false, nil
	}
	if err != nil {
		return Bounty{}, false, err
	}
	return b, true, nil
}

// ListBounties returns every bounty for the admin queue/history view: active
// first, then pending by deadline, then won newest-first.
func (s *Store) ListBounties(ctx context.Context) ([]Bounty, error) {
	rows, err := s.pool.Query(ctx, bountySelect+`
		ORDER BY
			CASE status WHEN 'active' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END,
			CASE WHEN status = 'won' THEN won_at END DESC NULLS LAST,
			win_time ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Bounty{}
	for rows.Next() {
		var b Bounty
		if err := s.scanBounty(rows, &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateBounty adds a pending bounty to the queue.
func (s *Store) CreateBounty(ctx context.Context, skinImage, label string, winTime time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO bounties (skin_image, label, win_time, status)
		VALUES ($1, $2, $3, 'pending')
	`, skinImage, label, winTime)
	return err
}

// UpdateBounty edits a not-yet-won bounty's editable fields. A "" skinImage
// leaves the image unchanged (the admin edited only the label/deadline). Won
// bounties are immutable (the WHERE clause excludes them).
func (s *Store) UpdateBounty(ctx context.Context, id int64, skinImage, label string, winTime time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE bounties
		   SET label = $2,
		       win_time = $3,
		       skin_image = CASE WHEN $4 = '' THEN skin_image ELSE $4 END
		 WHERE id = $1 AND status <> 'won'
	`, id, label, winTime, skinImage)
	return err
}

// DeleteBounty removes a pending bounty (active/won bounties are kept: the active
// one is in play, won ones are history).
func (s *Store) DeleteBounty(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM bounties WHERE id = $1 AND status = 'pending'`, id)
	return err
}

// FinalizeDueBounties advances the bounty queue: it closes out the active bounty
// once its win_time has passed (recording the window's winner) and promotes the
// next pending bounty, also promoting a pending bounty when none is active yet.
// It loops so a backlog (several deadlines crossed while the process was down)
// is caught up in one pass, and so the newly-promoted bounty is itself finalized
// if it too is already due. Returns the number of bounties newly finalized.
//
// Windows tile contiguously: a promoted bounty's window starts at the previous
// bounty's win_time (or now for the first-ever activation), so every game is
// attributed to exactly one bounty.
func (s *Store) FinalizeDueBounties(ctx context.Context, now time.Time) (int, error) {
	finalized := 0
	for {
		advanced, didFinalize, err := s.advanceBounty(ctx, now)
		if err != nil {
			return finalized, err
		}
		if didFinalize {
			finalized++
		}
		if !advanced {
			return finalized, nil
		}
	}
}

// advanceBounty performs one step of the queue under a row lock, in a tx:
//   - active bounty due → finalize it (record window winner), then promote next.
//   - no active bounty → promote the earliest pending one.
//
// advanced reports whether anything changed (drives the catch-up loop);
// didFinalize reports whether a bounty was closed out (vs. only a promotion).
func (s *Store) advanceBounty(ctx context.Context, now time.Time) (advanced, didFinalize bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)

	// Window start for the next promotion: the just-closed bounty's win_time so
	// windows are contiguous; falls back to now for the first-ever activation.
	promoteFrom := now

	var active Bounty
	err = s.scanBounty(tx.QueryRow(ctx, bountySelect+` WHERE status = 'active' FOR UPDATE`), &active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No active bounty — fall through to promote the earliest pending one.
	case err != nil:
		return false, false, err
	default:
		if active.WinTime.After(now) {
			return false, false, nil // active but not due yet — nothing to do
		}
		// Active bounty is due: settle it over its window [activated_at, win_time].
		start := active.WinTime
		if active.ActivatedAt != nil {
			start = *active.ActivatedAt
		}
		winnerID, winnerName, wins, err := windowWinner(ctx, tx, start, active.WinTime)
		if err != nil {
			return false, false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE bounties
			   SET status = 'won', winner_id = $2, winner_name = $3,
			       winner_wins = $4, won_at = $5
			 WHERE id = $1
		`, active.ID, nullIfEmpty(winnerID), winnerName, wins, now); err != nil {
			return false, false, err
		}
		didFinalize = true
		promoteFrom = active.WinTime
	}

	// Promote the earliest pending bounty (if any), beginning its window now.
	tag, err := tx.Exec(ctx, `
		UPDATE bounties SET status = 'active', activated_at = $1
		 WHERE id = (
			SELECT id FROM bounties WHERE status = 'pending'
			 ORDER BY win_time ASC, id ASC LIMIT 1 FOR UPDATE SKIP LOCKED
		 )
	`, promoteFrom)
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	// Keep looping while we either finalized something or promoted something — a
	// freshly promoted bounty may itself be due, or another may now be promotable.
	return didFinalize || tag.RowsAffected() > 0, didFinalize, nil
}

// windowWinner returns the player who won the most games (placement 1) whose
// games ended in (start, end], with their display-name snapshot and win count.
// An empty window (no qualifying games) yields ("", "", 0, nil).
func windowWinner(ctx context.Context, tx pgx.Tx, start, end time.Time) (id, name string, wins int, err error) {
	var uname, disp *string
	err = tx.QueryRow(ctx, `
		SELECT gs.steam_id, p.username, p.display_name, COUNT(*)::INT
		  FROM game_standings gs
		  JOIN games g ON g.id = gs.game_id
		  LEFT JOIN players p ON p.steam_id = gs.steam_id
		 WHERE gs.placement = 1
		   AND g.ended_at > $1 AND g.ended_at <= $2
		 GROUP BY gs.steam_id, p.username, p.display_name
		 ORDER BY COUNT(*) DESC, gs.steam_id ASC
		 LIMIT 1
	`, start, end).Scan(&id, &uname, &disp, &wins)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", 0, nil
	}
	if err != nil {
		return "", "", 0, err
	}
	return id, pickName(uname, disp), wins, nil
}

// bountySelect is the shared column list / order for scanBounty.
const bountySelect = `
	SELECT id, skin_image, label, win_time, status, activated_at,
	       winner_id, winner_name, winner_wins, won_at, created_at
	  FROM bounties`

// scanBounty reads one bounty row (from QueryRow or an iterating Query).
func (s *Store) scanBounty(row pgx.Row, b *Bounty) error {
	var winnerID *string
	if err := row.Scan(&b.ID, &b.SkinImage, &b.Label, &b.WinTime, &b.Status,
		&b.ActivatedAt, &winnerID, &b.WinnerName, &b.WinnerWins, &b.WonAt, &b.CreatedAt); err != nil {
		return err
	}
	b.WinnerID = deref(winnerID)
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
