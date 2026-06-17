package ws

import (
	"encoding/json"
	"testing"
)

// drainFor reads queued frames for a client and returns the idents of any
// `achievement` frames seen (other frame types — e.g. the hello on register —
// are ignored).
func drainAchievements(t *testing.T, c *Client) []string {
	t.Helper()
	var out []string
	for {
		select {
		case b := <-c.send:
			var f achievementWire
			if json.Unmarshal(b, &f) == nil && f.T == "achievement" {
				out = append(out, f.Ident)
			}
		default:
			return out
		}
	}
}

func TestFireAchievementMatchesByIP(t *testing.T) {
	h := NewHub(nil)
	// Two connections share an IP (e.g. a player poking the backend in a browser
	// next to the game), a third is elsewhere.
	a := NewClient(nil, "1", "aa", "alice", "1.2.3.4", h)
	b := NewClient(nil, "2", "bb", "bob", "1.2.3.4", h)
	c := NewClient(nil, "3", "cc", "carol", "9.9.9.9", h)
	for _, cl := range []*Client{a, b, c} {
		h.register(cl)
	}

	if n := h.FireAchievement("1.2.3.4", "fart"); n != 2 {
		t.Fatalf("notified %d connections, want 2", n)
	}

	if got := drainAchievements(t, a); len(got) != 1 || got[0] != "fart" {
		t.Errorf("alice got %v, want [fart]", got)
	}
	if got := drainAchievements(t, b); len(got) != 1 || got[0] != "fart" {
		t.Errorf("bob got %v, want [fart]", got)
	}
	if got := drainAchievements(t, c); len(got) != 0 {
		t.Errorf("carol (other IP) got %v, want none", got)
	}
}

func TestFireAchievementNoMatchIsSilent(t *testing.T) {
	h := NewHub(nil)
	a := NewClient(nil, "1", "aa", "alice", "1.2.3.4", h)
	h.register(a)

	if n := h.FireAchievement("5.6.7.8", "hackerman"); n != 0 {
		t.Fatalf("notified %d connections for an absent IP, want 0", n)
	}
	if n := h.FireAchievement("", "hackerman"); n != 0 {
		t.Fatalf("notified %d connections for an empty IP, want 0", n)
	}
	if got := drainAchievements(t, a); len(got) != 0 {
		t.Errorf("alice got %v, want none", got)
	}
}
