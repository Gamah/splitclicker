package store

import (
	"context"
	"sync"
	"time"
)

// CacheLimit is the number of rows held per cached board — and the DB LIMIT used
// when (re)querying. The client only shows the top handful, so 15 is plenty.
const CacheLimit = 15

// LeaderboardCache serves the read-only boards (hourly, hours-won, sessions-won)
// from memory so the API never hits Postgres on a plain board read. It is
// refreshed explicitly via Refresh rather than on a timer: today that's once at
// startup and once per game ("session") end (see cmd/server wiring). The trigger
// is intentionally swappable — a TTL or a LISTEN/NOTIFY push would replace the
// per-session refresh later without touching the API handlers.
type LeaderboardCache struct {
	store *Store

	mu           sync.RWMutex
	hourly       []LeaderboardEntry
	hoursWon     []LeaderboardEntry
	sessionsWon  []LeaderboardEntry
	allTimeClick []LeaderboardEntry

	// Active bounty metadata, refreshed alongside the boards. bountyID scopes the
	// anticheat sanction ladder; bountyResolveMs is its winner-lock time (the
	// "ignored" countdown target). hasBounty is false when none is active.
	bountyID        int64
	bountyResolveMs int64
	hasBounty       bool
}

// NewLeaderboardCache builds an empty cache over st. Call Refresh once before
// serving so reads don't return empty boards.
func NewLeaderboardCache(st *Store) *LeaderboardCache {
	return &LeaderboardCache{
		store:        st,
		hourly:       []LeaderboardEntry{},
		hoursWon:     []LeaderboardEntry{},
		sessionsWon:  []LeaderboardEntry{},
		allTimeClick: []LeaderboardEntry{},
	}
}

// Refresh re-queries all four boards (each capped at CacheLimit) and swaps them
// in atomically under the write lock. The three competitive boards are scoped to
// the active bounty's window (its activated_at); with no active bounty they fall
// back to all-time (and the points board to the current hour), preserving the
// pre-bounty behaviour. All-time clickers always spans the whole DB. On any query
// error the previous snapshot is kept (a stale board beats an empty one).
func (c *LeaderboardCache) Refresh(ctx context.Context) error {
	now := time.Now().UTC()
	currentHour := now.Truncate(time.Hour)

	// Window start: the active bounty's activation. No bounty → all-time for the
	// won boards; the points board falls back to the current hour.
	since := time.Time{}
	pointsSince := currentHour
	var bountyID, bountyResolveMs int64
	hasBounty := false
	if b, ok, err := c.store.ActiveBounty(ctx); err != nil {
		return err
	} else if ok {
		hasBounty = true
		bountyID = b.ID
		bountyResolveMs = b.WinTime.UnixMilli()
		if b.ActivatedAt != nil {
			since = b.ActivatedAt.UTC()
			pointsSince = since.Truncate(time.Hour)
		}
	}

	hourly, err := c.store.HourlyLeaderboard(ctx, pointsSince, CacheLimit)
	if err != nil {
		return err
	}
	hoursWon, err := c.store.HoursWonLeaderboard(ctx, since, currentHour, CacheLimit)
	if err != nil {
		return err
	}
	sessionsWon, err := c.store.SessionsWonLeaderboard(ctx, since, CacheLimit)
	if err != nil {
		return err
	}
	allTime, err := c.store.AllTimeClickers(ctx, CacheLimit)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.hourly, c.hoursWon, c.sessionsWon, c.allTimeClick = hourly, hoursWon, sessionsWon, allTime
	c.bountyID, c.bountyResolveMs, c.hasBounty = bountyID, bountyResolveMs, hasBounty
	c.mu.Unlock()
	return nil
}

// ActiveBountyMeta returns the cached active bounty's id and winner-lock time
// (epoch ms), and whether one is active. Used by the anticheat sanction ladder.
func (c *LeaderboardCache) ActiveBountyMeta() (id, resolveMs int64, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bountyID, c.bountyResolveMs, c.hasBounty
}

// Hourly/HoursWon/SessionsWon return the cached board, truncated to limit rows.
// The returned slice shares the cache's backing array (read-only — Refresh swaps
// in a fresh slice rather than mutating), so callers must not modify it.
func (c *LeaderboardCache) Hourly(limit int) []LeaderboardEntry {
	c.mu.RLock()
	b := c.hourly
	c.mu.RUnlock()
	return clampBoard(b, limit)
}

func (c *LeaderboardCache) HoursWon(limit int) []LeaderboardEntry {
	c.mu.RLock()
	b := c.hoursWon
	c.mu.RUnlock()
	return clampBoard(b, limit)
}

func (c *LeaderboardCache) SessionsWon(limit int) []LeaderboardEntry {
	c.mu.RLock()
	b := c.sessionsWon
	c.mu.RUnlock()
	return clampBoard(b, limit)
}

func (c *LeaderboardCache) AllTimeClickers(limit int) []LeaderboardEntry {
	c.mu.RLock()
	b := c.allTimeClick
	c.mu.RUnlock()
	return clampBoard(b, limit)
}

func clampBoard(b []LeaderboardEntry, limit int) []LeaderboardEntry {
	if limit < 0 {
		limit = 0
	}
	if limit > len(b) {
		limit = len(b)
	}
	return b[:limit]
}
