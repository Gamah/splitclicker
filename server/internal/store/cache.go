package store

import (
	"context"
	"sync"
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

	mu          sync.RWMutex
	hourly      []LeaderboardEntry
	hoursWon    []LeaderboardEntry
	sessionsWon []LeaderboardEntry
}

// NewLeaderboardCache builds an empty cache over st. Call Refresh once before
// serving so reads don't return empty boards.
func NewLeaderboardCache(st *Store) *LeaderboardCache {
	return &LeaderboardCache{
		store:       st,
		hourly:      []LeaderboardEntry{},
		hoursWon:    []LeaderboardEntry{},
		sessionsWon: []LeaderboardEntry{},
	}
}

// Refresh re-queries all three boards (each capped at CacheLimit) and swaps them
// in atomically under the write lock. On any query error the previous snapshot
// is kept (a stale board beats an empty one) and the error is returned.
func (c *LeaderboardCache) Refresh(ctx context.Context) error {
	hourly, err := c.store.HourlyLeaderboard(ctx, CacheLimit)
	if err != nil {
		return err
	}
	hoursWon, err := c.store.HoursWonLeaderboard(ctx, CacheLimit)
	if err != nil {
		return err
	}
	sessionsWon, err := c.store.SessionsWonLeaderboard(ctx, CacheLimit)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.hourly, c.hoursWon, c.sessionsWon = hourly, hoursWon, sessionsWon
	c.mu.Unlock()
	return nil
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

func clampBoard(b []LeaderboardEntry, limit int) []LeaderboardEntry {
	if limit < 0 {
		limit = 0
	}
	if limit > len(b) {
		limit = len(b)
	}
	return b[:limit]
}
