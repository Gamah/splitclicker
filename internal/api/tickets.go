package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// wsTicketTTL bounds how long a one-time WS ticket is valid: the client mints
// one and immediately opens the socket, so this is generous.
const wsTicketTTL = 60 * time.Second

// identity is the resolved player a ticket stands for. It is proven once over
// HTTP (Facepunch token) so the WS upgrade carries only ?ticket=, never the
// SteamID/token in the URL (which would leak into proxy/access logs).
type identity struct {
	SteamID  string
	Tag      string
	Username string
}

type ticketEntry struct {
	id  identity
	exp time.Time
}

// ticketStore is a tiny single-use token map: Take deletes on read, and each
// write opportunistically sweeps expired entries (volume here is tiny).
type ticketStore struct {
	mu sync.Mutex
	m  map[string]ticketEntry
}

func newTicketStore() *ticketStore { return &ticketStore{m: map[string]ticketEntry{}} }

func (s *ticketStore) Put(ticket string, id identity, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, e := range s.m {
		if now.After(e.exp) {
			delete(s.m, k)
		}
	}
	s.m[ticket] = ticketEntry{id: id, exp: now.Add(ttl)}
}

// Take returns the identity and removes the ticket; ok is false if missing/expired.
func (s *ticketStore) Take(ticket string) (identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[ticket]
	if !ok {
		return identity{}, false
	}
	delete(s.m, ticket)
	if time.Now().After(e.exp) {
		return identity{}, false
	}
	return e.id, true
}

func randToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
