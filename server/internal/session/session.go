// Package session holds the public player tag helper. Identity itself is the
// Steam account (see internal/steam); the SteamID is the authoritative key, the
// tag is what we expose publicly.
//
// There is deliberately no client-supplied username: identity is purely the Steam
// account, the board name is the Steam display name, and the tag is SteamID-derived
// (see the note in api/router.go's auth and the CLAUDE.md decision). The PlayerTag
// signature keeps a name parameter only because the now-vestigial players.username
// column is still read through it; in practice it is always "".
package session

import (
	"crypto/sha256"
	"encoding/hex"
)

// PlayerTag is the public identifier broadcast in place of the raw SteamID64:
// the first 8 hex chars of sha256(steamID + username). The SteamID is a stable
// account identifier we'd rather not expose verbatim in every leaderboard frame;
// the tag is stable per account and reveals nothing reversible. username is the
// vestigial (always-empty) handle, so the tag is effectively sha256(steamID).
func PlayerTag(steamID, username string) string {
	sum := sha256.Sum256([]byte(steamID + username))
	return hex.EncodeToString(sum[:])[:8]
}
