// Package steam validates s&box (Facepunch) auth tokens server-side so the
// backend can trust a client-supplied SteamID64 instead of taking it on faith.
//
// The s&box client generates a token with Sandbox.Services.Auth.GetToken and
// ships it to us alongside its SteamID64. We forward {steamid, token} to
// Facepunch; the token is only valid if Facepunch reports Status=="ok" AND
// echoes back the same SteamId we were given. See sbox-docs services/auth-tokens.
//
// This is the ONLY auth path splitclicker uses — there is no Steam OpenID web
// sign-in (that lives in rotaliate; the s&box client never needs it).
package steam

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// endpoint and client are package vars so tests can point at a stub server and
// so the outbound call always has a bounded timeout (we fail closed on error).
var (
	endpoint = "https://public.facepunch.com/sbox/auth/token"
	client   = &http.Client{Timeout: 5 * time.Second}
)

type validateResponse struct {
	SteamID int64  `json:"SteamId"`
	Status  string `json:"Status"`
}

// ValidateToken reports whether token proves ownership of steamID64. steamID64
// is the decimal SteamID64 string the client sent. Any transport error, non-200
// response, bad status, or SteamId mismatch returns (false, err|nil) — callers
// must treat a non-true result as "deny", never as "allow on error".
func ValidateToken(ctx context.Context, steamID64, token string) (bool, error) {
	steamID, err := strconv.ParseInt(steamID64, 10, 64)
	if err != nil {
		return false, fmt.Errorf("steam: bad steamid %q: %w", steamID64, err)
	}
	payload, _ := json.Marshal(map[string]any{"steamid": steamID, "token": token})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("steam: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("steam: validate request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("steam: validate status %d", resp.StatusCode)
	}

	var vr validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&vr); err != nil {
		return false, fmt.Errorf("steam: decode response: %w", err)
	}
	// Both checks matter: a valid token for a *different* Steam account must not
	// authorise this steamID.
	return vr.Status == "ok" && vr.SteamID == steamID, nil
}
