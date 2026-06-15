// Package session holds small identity helpers: the public player tag and
// username validation. Identity itself is the Steam account (see internal/steam);
// the SteamID is the authoritative key, the tag is what we expose publicly.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// usernameRe enforces: 3–20 chars, alphanumeric + underscore, no leading/trailing underscore.
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_]{1,18}[a-zA-Z0-9]$`)

// reservedNames are matched exactly (case-insensitive): impersonation only
// matters when the term is the whole name. Authority is keyed by SteamID, never
// by username, so we don't need substring matching here.
var reservedNames = []string{
	"admin", "moderator", "splitclicker", "system", "null", "root",
}

// profanityWords stay a substring match (slur evasion via embedding).
var profanityWords = []string{
	"nigger", "faggot", "retard", // extend as needed
}

// PlayerTag is the public identifier broadcast in place of the raw SteamID64:
// the first 8 hex chars of sha256(steamID + username). The SteamID is a stable
// account identifier we'd rather not expose verbatim in every leaderboard frame;
// the tag is stable per (account, name) and reveals nothing reversible.
func PlayerTag(steamID, username string) string {
	sum := sha256.Sum256([]byte(steamID + username))
	return hex.EncodeToString(sum[:])[:8]
}

// ValidateUsername returns nil if the username is acceptable.
func ValidateUsername(name string) error {
	if !usernameRe.MatchString(name) {
		return errors.New("username must be 3–20 characters, alphanumeric or underscore, not starting/ending with underscore")
	}
	lower := strings.ToLower(name)
	for _, word := range reservedNames {
		if lower == word {
			return errors.New("username is reserved")
		}
	}
	for _, word := range profanityWords {
		if strings.Contains(lower, word) {
			return errors.New("username contains a disallowed word")
		}
	}
	return nil
}
