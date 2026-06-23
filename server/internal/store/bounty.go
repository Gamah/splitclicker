package store

import (
	"context"
	"errors"
	"time"

	"github.com/gamah/splitclicker/internal/session"
	"github.com/jackc/pgx/v5"
)

// Bounty is one skin offered for a timeframe, with its (eventual) winner. See
// migrations/00008_bounties.sql for the lifecycle (pending → active → won).
type Bounty struct {
	ID          int64
	SkinImage   string
	InspectLink string // CS2 inspect link; "" = use the uploaded SkinImage only
	Label       string
	WinTime     time.Time
	Status      string // pending | active | won
	ActivatedAt *time.Time
	WinnerID    string // "" until finalized (or empty window)
	WinnerName  string
	WinnerWins  int
	WonAt       *time.Time
	CreatedAt   time.Time
	Archived    bool // hidden from every client-facing read (admin-only visibility)
}

// ActiveBounty returns the currently active bounty (the skin shown + the
// countdown). ok is false when no bounty is active (e.g. before any is added or
// after the queue has drained) — callers fall back to config.json/env. An
// archived bounty is treated as absent here, so archiving the active one makes
// the client show the "no active bounty" state without disturbing the queue.
func (s *Store) ActiveBounty(ctx context.Context) (b Bounty, ok bool, err error) {
	err = s.scanBounty(s.pool.QueryRow(ctx, bountySelect+` WHERE status = 'active' AND NOT archived LIMIT 1`), &b)
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

// BountyWindow is a settled or in-flight bounty reduced to the time span it
// scopes — used to populate the admin "filter by bounty" toggle and to resolve a
// selected filter to a store.Window. Start is the bounty's activation; End is its
// win_time once won, or now while it is still active.
type BountyWindow struct {
	ID     int64
	Label  string
	Status string // active | won
	Start  time.Time
	End    time.Time
}

// Window returns the (Start, End] span this bounty scopes, as a store.Window.
func (b BountyWindow) Window() Window {
	return Window{Start: b.Start, End: b.End}
}

// SelectableBounties returns the bounties that have a real time window — the
// active one and every won one (pending bounties have no window yet) — newest
// first with the active one on top. These are the choices in the dashboard's
// bounty filter.
func (s *Store) SelectableBounties(ctx context.Context) ([]BountyWindow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, label, status, activated_at,
		       CASE WHEN status = 'won' THEN win_time ELSE now() END
		FROM bounties
		WHERE status IN ('active', 'won') AND activated_at IS NOT NULL
		ORDER BY CASE status WHEN 'active' THEN 0 ELSE 1 END,
		         won_at DESC NULLS LAST, activated_at DESC, id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []BountyWindow{}
	for rows.Next() {
		var b BountyWindow
		if err := rows.Scan(&b.ID, &b.Label, &b.Status, &b.Start, &b.End); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateBounty adds a pending bounty to the queue. inspectLink may be "" (the
// skin is given by the uploaded image only) and skinImage may be "" (the skin is
// given by the inspect link only) — the caller enforces that at least one is set.
func (s *Store) CreateBounty(ctx context.Context, skinImage, inspectLink, label string, winTime time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO bounties (skin_image, inspect_link, label, win_time, status)
		VALUES ($1, $2, $3, $4, 'pending')
	`, skinImage, inspectLink, label, winTime)
	return err
}

// UpdateBounty edits a not-yet-won bounty's editable fields. A "" skinImage
// leaves the image unchanged (the admin edited only the label/deadline/link); the
// inspect link is always set as given (clear it by submitting an empty link). Won
// bounties are immutable (the WHERE clause excludes them).
func (s *Store) UpdateBounty(ctx context.Context, id int64, skinImage, inspectLink, label string, winTime time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE bounties
		   SET label = $2,
		       win_time = $3,
		       inspect_link = $5,
		       skin_image = CASE WHEN $4 = '' THEN skin_image ELSE $4 END
		 WHERE id = $1 AND status <> 'won'
	`, id, label, winTime, skinImage, inspectLink)
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

// WonBounty is a settled bounty plus its winner, reduced to what the client's
// "previous winner" panel needs: the skin (inspect link and/or image) and the
// winner's public identity. WinnerTag is the same public tag the client knows
// itself by (so it can highlight "you won"), computed from the winner's current
// username; it (and WinnerID/Name) are empty when the window had no winner.
type WonBounty struct {
	ID          int64
	Label       string
	SkinImage   string
	InspectLink string
	WinnerTag   string
	WinnerID    string
	WinnerName  string
	WinnerWins  int
	WonAt       time.Time
}

// RecentWonBounties returns up to limit settled bounties, newest-won first — the
// history the client rebuilds the "previous winner" panel(s) from. The winner's
// tag is derived from their current username (LEFT JOIN, so a deleted player just
// yields an empty winner).
func (s *Store) RecentWonBounties(ctx context.Context, limit int) ([]WonBounty, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.label, b.skin_image, b.inspect_link,
		       b.winner_id, b.winner_name, p.username, b.winner_wins, b.won_at
		  FROM bounties b
		  LEFT JOIN players p ON p.steam_id = b.winner_id
		 WHERE b.status = 'won' AND NOT b.archived
		 ORDER BY b.won_at DESC NULLS LAST, b.id DESC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []WonBounty{}
	for rows.Next() {
		var b WonBounty
		var winnerID, username *string
		var wonAt *time.Time
		if err := rows.Scan(&b.ID, &b.Label, &b.SkinImage, &b.InspectLink,
			&winnerID, &b.WinnerName, &username, &b.WinnerWins, &wonAt); err != nil {
			return nil, err
		}
		b.WinnerID = deref(winnerID)
		if b.WinnerID != "" && username != nil && *username != "" {
			b.WinnerTag = session.PlayerTag(b.WinnerID, *username)
		}
		if wonAt != nil {
			b.WonAt = *wonAt
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BountySkinImage returns one bounty's uploaded skin image filename ("" if it's a
// link-only bounty or the id is unknown) — used to serve a specific (past)
// bounty's image to the previous-winner panel.
func (s *Store) BountySkinImage(ctx context.Context, id int64) (string, error) {
	var img string
	err := s.pool.QueryRow(ctx, `SELECT skin_image FROM bounties WHERE id = $1`, id).Scan(&img)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return img, err
}

// SetBountyArchived flips a bounty's archived flag. Archiving hides it from every
// client-facing read (active skin + previous-winner history) without touching its
// lifecycle status; un-archiving restores it. Works on a bounty in any status so
// the host can hide the live one to test the empty state, then bring it back.
func (s *Store) SetBountyArchived(ctx context.Context, id int64, archived bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE bounties SET archived = $2 WHERE id = $1`, id, archived)
	return err
}

// bountySelect is the shared column list / order for scanBounty.
const bountySelect = `
	SELECT id, skin_image, inspect_link, label, win_time, status, activated_at,
	       winner_id, winner_name, winner_wins, won_at, created_at, archived
	  FROM bounties`

// scanBounty reads one bounty row (from QueryRow or an iterating Query).
func (s *Store) scanBounty(row pgx.Row, b *Bounty) error {
	var winnerID *string
	if err := row.Scan(&b.ID, &b.SkinImage, &b.InspectLink, &b.Label, &b.WinTime, &b.Status,
		&b.ActivatedAt, &winnerID, &b.WinnerName, &b.WinnerWins, &b.WonAt, &b.CreatedAt, &b.Archived); err != nil {
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
