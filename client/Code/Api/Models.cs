using System.Collections.Generic;
using System.Text.Json.Serialization;

namespace Splitclicker.Api;

// POST /api/v1/auth response: the public tag/username plus a single-use WS ticket
// (mint immediately before connecting; expires after ttl_ms).
public record AuthResponse(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username,
	[property: JsonPropertyName( "ticket" )] string Ticket,
	[property: JsonPropertyName( "ttl_ms" )] long TtlMs
);

// One row of the hourly leaderboard (GET /api/v1/leaderboard/hourly) and of the
// standings embedded in round_result / game_over.
public record Standing(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username,
	[property: JsonPropertyName( "points" )] int Points
);

// --- WebSocket server→client frames (the "t" field selects the shape) ---

public record HelloYou(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username
);

public record HelloGame(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "of" )] int Of,
	[property: JsonPropertyName( "phase" )] string Phase
);

public record HelloMsg(
	[property: JsonPropertyName( "you" )] HelloYou You,
	[property: JsonPropertyName( "game" )] HelloGame Game
);

public record PendingMsg(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "of" )] int Of
);

// nonce is a hex string (an unguessable 64-bit token); echo it back verbatim in
// the click frame — never parse/reformat it.
public record ArmedMsg(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "seq" )] int Seq,
	[property: JsonPropertyName( "nonce" )] string Nonce
);

public record YouResult(
	[property: JsonPropertyName( "points_delta" )] int PointsDelta,
	[property: JsonPropertyName( "round_id" )] string RoundId
);

public record RoundResultMsg(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "of" )] int Of,
	[property: JsonPropertyName( "winners" )] List<Standing> Winners,
	[property: JsonPropertyName( "standings" )] List<Standing> Standings,
	[property: JsonPropertyName( "you" )] YouResult You
);

public record YouGameOver(
	[property: JsonPropertyName( "placement" )] int Placement,
	[property: JsonPropertyName( "won" )] bool Won,
	[property: JsonPropertyName( "game_id" )] string GameId
);

public record GameOverMsg(
	[property: JsonPropertyName( "standings" )] List<Standing> Standings,
	[property: JsonPropertyName( "you" )] YouGameOver You
);
