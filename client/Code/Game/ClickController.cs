using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Threading.Tasks;
using Sandbox;
using Splitclicker.Api;
using Splitclicker.Audio;
using Splitclicker.Ws;

namespace Splitclicker.Game;

public enum GamePhase
{
	Connecting,
	Waiting,      // connected mid-round: sitting out until the next arm (can't score this one)
	Pending,      // arming — button dormant
	Armed,        // live — race is open
	Result,       // round leaderboard
	GameOver,     // final standings
	Disconnected, // lost the socket; backing off to reconnect
}

// Root component: owns the WebSocket lifecycle and the authoritative client-side
// view of the game. The whole UI reads this single instance. The click path is
// deliberately trivial — capture the press, send the nonce frame, nothing else.
public sealed class ClickController : Component
{
	public static ClickController Instance { get; private set; }

	/// <summary>Backend root, editable in the scene inspector. Leave blank to use
	/// the baked-in production URL (<see cref="ApiClient.ProdUrl"/>); set it to e.g.
	/// http://localhost:8080 for a local play-test. Applied to ApiClient at startup.</summary>
	[Property] public string BackendUrl { get; set; } = "";

	/// <summary>API version to talk to, editable in the scene inspector. "v4" is the
	/// real game (anticheat test gate + the cooldown/ignored sanction ladder); "v3"
	/// understands the test gate but not the sanction countdowns; "v2"/"v1" exercise
	/// the legacy/troll path the server gives clients below its live version. LEAVE
	/// BLANK to use raw, unversioned paths (bare /ws) — for the live old master
	/// backend. Applied to <see cref="ApiClient.ApiVersion"/> at startup.</summary>
	[Property] public string ApiVersion { get; set; } = "v4";

	public GamePhase Phase { get; private set; } = GamePhase.Connecting;
	public int Round { get; private set; }
	public int Of { get; private set; }
	public string Tag { get; private set; } = "";
	public string Username { get; private set; } = "";
	public List<Standing> Standings { get; private set; } = new();
	public List<Standing> Winners { get; private set; } = new();

	/// <summary>Connected players (open server connections) and the scoring slots
	/// this round (N = a multiple of the player count). Shown pre-click.</summary>
	public int Players { get; private set; }
	public int ClicksToWin { get; private set; }
	/// <summary>This connection's current arm-delay penalty in ms (the spam
	/// deterrent), 0 for honest clients. Surfaced so the player sees the throttle.
	/// Counted locally as the player idle-clicks (see <see cref="SendClick"/>) so it
	/// updates live; the armed frame's authoritative value then overwrites it.</summary>
	public int PenaltyMs { get; private set; }

	/// <summary>Host-editable broadcast note (shown orange under the throttle line);
	/// empty = none. Set from the dev_note frame (once per game) and the hello
	/// snapshot, and only ever changed by those — it persists across rounds and
	/// reconnects until the server sends an empty note.</summary>
	public string DevNote { get; private set; } = "";

	/// <summary>Click frames actually sent to the API during the current/just-ended
	/// CLICK! phase. Reset on each arm; shown under the button.</summary>
	public int ClicksSent { get; private set; }

	/// <summary>The arming-window bounds (seconds) from the server config; the
	/// per-round delay itself stays secret. Shown while the round is arming.</summary>
	public int ArmMinSec { get; private set; }
	public int ArmMaxSec { get; private set; }

	/// <summary>Anticheat test gate. When HasTest is true the player failed an
	/// end-of-round check and is benched until they answer TestPrompt correctly: the
	/// server withholds the armed signal until then. TestId must be echoed in the
	/// answer. TestMessage explains which check fired. Set by the `test` frame;
	/// cleared by it (correct answer) or on arm.</summary>
	public bool HasTest { get; private set; }
	public string TestId { get; private set; } = "";
	public string TestPrompt { get; private set; } = "";
	public string TestMessage { get; private set; } = "";

	/// <summary>Anticheat sanction ladder (beyond the test gate). SanctionState is
	/// "cooldown" (a timed cooldown after too many flags) or "ignored" (sidelined for
	/// the rest of the bounty), or "" when not sanctioned. SanctionUntilMs is the
	/// epoch ms the state ends — the UI shows a countdown to it — and SanctionMessage
	/// is the line to display. Set/cleared by the `test` frame's state field.</summary>
	public string SanctionState { get; private set; } = "";
	public long SanctionUntilMs { get; private set; }
	public string SanctionMessage { get; private set; } = "";

	/// <summary>True only while a valid click can score — drives both the button's
	/// enabled state and scoring eligibility from one source.</summary>
	public bool CanClick => Phase == GamePhase.Armed && !string.IsNullOrEmpty( _nonce );

	static readonly JsonSerializerOptions JsonOpts = new() { PropertyNameCaseInsensitive = true };

	// Idle clicks sent this round; drives the locally-counted throttle estimate.
	int _idleClicks;

	WsClient _ws;
	string _nonce;
	bool _connecting;
	int _reconnectAttempt;
	float _reconnectAt;

	protected override void OnAwake()
	{
		Instance = this;
		if ( !string.IsNullOrWhiteSpace( BackendUrl ) )
			ApiClient.BaseUrl = BackendUrl.TrimEnd( '/' );
		// Always apply, even when blank: an empty version means raw, unversioned
		// paths (/api/… and /ws) for talking to a legacy backend like the live old
		// master (its socket is bare /ws). Skipping blank would keep the v2 default.
		ApiClient.ApiVersion = (ApiVersion ?? "").Trim().Trim( '/' );
		_ws = GameObject.Components.GetOrCreate<WsClient>();
		_ws.OnMessage = OnMessage;
		_ws.OnDone = OnDisconnected;
	}

	protected override void OnStart() => _ = ConnectFlow();

	protected override void OnUpdate()
	{
		// Jittered reconnect: only re-attempt once the backoff window elapses.
		if ( Phase == GamePhase.Disconnected && !_connecting && RealTime.Now >= _reconnectAt )
			_ = ConnectFlow();
	}

	// SendClick is the hot path. While armed, fire the nonce frame (it scores) and
	// count it. In EVERY other connected phase — dormant, mid-round stand-by, result,
	// game over — send an idle click with no nonce: it scores nothing but is a "bad"
	// click the server penalises (the escalating arm-delay), so the player sees the
	// throttle they're inflicting on themselves no matter when they mash. Returns true
	// when a bad click was actually sent (so the HUD can pop a "+ PENALTY"); false for
	// a scoring click or when there's no socket to penalise it.
	public bool SendClick()
	{
		// Local audio feedback, independent of socket state and mirroring exactly what the
		// button shows: the "click" blip only while the race is live (the CLICK! window),
		// the "throttle" nope in every other state. Plays even while disconnected.
		if ( CanClick ) SoundPlayer.PlayClick();
		else SoundPlayer.PlayThrottle();

		if ( _ws == null || !_ws.Connected ) return false;
		if ( CanClick )
		{
			_ = _ws.Send( $"{{\"t\":\"click\",\"nonce\":\"{_nonce}\"}}" );
			ClicksSent++;
			return false;
		}

		// Any non-armed phase: an idle (bad) click. Send it so the server sees and
		// penalises it, even mid-round / between games.
		_ = _ws.Send( "{\"t\":\"click\",\"nonce\":\"\"}" );
		// Mirror the server's escalating bad-click penalty locally so the throttle
		// climbs the instant the player mashes; the next armed frame's authoritative
		// value overwrites this estimate (and resets the count — see OnMessage).
		_idleClicks++;
		PenaltyMs = IdlePenaltyMs( _idleClicks );
		return true;
	}

	// Mirror of the server's bad-click penalty (game.idlePenalty): the kth bad click
	// since the last arm adds base+step·(k−1) ms. base/step are server-configured and
	// arrive in the hello frame; until then we use the 500/100 default so the throttle
	// estimate still drives the UI from the very first click.
	int _penaltyBaseMs = 500;
	int _penaltyStepMs = 100;
	int IdlePenaltyMs( int n ) => n <= 0 ? 0 : _penaltyBaseMs * n + _penaltyStepMs * n * ( n - 1 ) / 2;

	// Submit the player's answer to the current anticheat test. Fire-and-forget over
	// the socket, echoing the test id. A correct answer earns a `test` cleared frame
	// (un-benched); a wrong one earns a fresh `test`. No-op without an active test.
	public void SubmitTestAnswer( string answer )
	{
		if ( !HasTest || _ws == null || !_ws.Connected ) return;
		// JsonSerializer quotes/escapes both fields so arbitrary answer text is safe.
		var id = JsonSerializer.Serialize( TestId );
		var ans = JsonSerializer.Serialize( answer ?? "" );
		_ = _ws.Send( $"{{\"t\":\"test_answer\",\"id\":{id},\"answer\":{ans}}}" );
	}

	void ClearTest()
	{
		HasTest = false;
		TestId = "";
		TestPrompt = "";
		TestMessage = "";
	}

	void ClearSanction()
	{
		SanctionState = "";
		SanctionUntilMs = 0;
		SanctionMessage = "";
	}

	async Task ConnectFlow()
	{
		if ( _connecting ) return;
		_connecting = true;
		Phase = GamePhase.Connecting;

		try
		{
			var pd = PlayerData.Load();
			var auth = await ApiClient.Auth( string.IsNullOrEmpty( pd.Username ) ? null : pd.Username );
			if ( auth == null )
			{
				Fail();
				return;
			}
			Tag = auth.Tag;
			Username = auth.Username;
			pd.Username = auth.Username;
			pd.PlayerTag = auth.Tag;
			pd.Save();

			await _ws.Connect( ApiClient.WsUrl( auth.Ticket ) );
			_reconnectAttempt = 0;
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] connect failed: {e.Message}" );
			Fail();
			return;
		}
		finally
		{
			_connecting = false;
		}
	}

	void Fail()
	{
		Phase = GamePhase.Disconnected;
		ScheduleReconnect();
		_connecting = false;
	}

	void OnDisconnected()
	{
		Phase = GamePhase.Disconnected;
		_nonce = null;
		ScheduleReconnect();
	}

	void ScheduleReconnect()
	{
		_reconnectAttempt++;
		// Exponential base capped at 15s, plus up to 50% jitter so a server restart
		// doesn't land every client's reconnect in the same instant (PLAN §3.5e).
		float baseDelay = MathF.Min( MathF.Pow( 2f, _reconnectAttempt - 1 ), 15f );
		float jitter = baseDelay * 0.5f * System.Random.Shared.NextSingle();
		_reconnectAt = RealTime.Now + baseDelay + jitter;
	}

	void OnMessage( string json )
	{
		try
		{
			using var doc = JsonDocument.Parse( json );
			if ( !doc.RootElement.TryGetProperty( "t", out var tEl ) ) return;
			switch ( tEl.GetString() )
			{
				case "hello":
					var h = Deser<HelloMsg>( json );
					Tag = h.You.Tag;
					Username = h.You.Username;
					Round = h.Game.Round;
					Of = h.Game.Of;
					Players = h.Game.Players;
					ClicksToWin = h.Game.Clicks;
					ArmMinSec = h.Game.ArmMin;
					ArmMaxSec = h.Game.ArmMax;
					// Adopt the server's penalty escalation (keep the 500/100 default if it
					// didn't send one) so the local throttle estimate matches the authority.
					if ( h.Game.PenaltyBaseMs > 0 ) _penaltyBaseMs = h.Game.PenaltyBaseMs;
					if ( h.Game.PenaltyStepMs > 0 ) _penaltyStepMs = h.Game.PenaltyStepMs;
					DevNote = h.Game.DevNote ?? "";
					Phase = PhaseFrom( h.Game.Phase );
					break;

				case "round_pending":
					var p = Deser<PendingMsg>( json );
					Round = p.Round;
					Of = p.Of;
					Players = p.Players;
					ClicksToWin = p.Clicks;
					// A new game's first round is arming: clear the previous game's session
					// standings now (the arming signal for round 1) so the board doesn't keep
					// showing the last game's totals through this game's first round — it's
					// repopulated by this round's round_result. Fixes the session board
					// "lagging a game behind" after game_over.
					if ( p.Round == 1 ) Standings = new();
					// NB: the throttle is NOT reset here — bad clicks accrue across phases
					// (result/game-over/intermission included) and are forgiven only at the
					// next arm, mirroring the server. The armed frame resets _idleClicks.
					Phase = GamePhase.Pending;
					_nonce = null;
					SoundPlayer.PlayArming();
					break;

				case "armed":
					var a = Deser<ArmedMsg>( json );
					Round = a.Round;
					Players = a.Players;
					ClicksToWin = a.Clicks;
					PenaltyMs = a.PenaltyMs;
					_idleClicks = 0; // the server forgave the accrued bad clicks at this arm
					ClicksSent = 0;  // fresh CLICK! phase: start the sent tally over
					_nonce = a.Nonce;
					Phase = GamePhase.Armed;
					// Receiving an arm means we're no longer benched — clear any stale test.
					ClearTest();
					SoundPlayer.PlayArmed();
					break;

				case "round_result":
					var r = Deser<RoundResultMsg>( json );
					Round = r.Round;
					Of = r.Of;
					Winners = r.Winners ?? new();
					Standings = r.Standings ?? new();
					Phase = GamePhase.Result;
					_nonce = null;
					SoundPlayer.PlayDisarm();
					AchievementTracker.OnRoundResult( r.You.PointsDelta, r.You.RoundId );
					break;

				case "game_over":
					var g = Deser<GameOverMsg>( json );
					Standings = g.Standings ?? new();
					Phase = GamePhase.GameOver;
					_nonce = null;
					SoundPlayer.PlayDisarm();
					// The final round folds into game_over (no round_result of its own), so
					// credit that round's points here too — same once-per-round-id guard.
					AchievementTracker.OnRoundResult( g.You.PointsDelta, g.You.RoundId );
					AchievementTracker.OnGameOver( g.You.Placement, g.You.Won, g.You.GameId );
					break;

				case "dev_note":
					var dn = Deser<DevNoteMsg>( json );
					DevNote = dn.Note ?? "";
					break;

				case "test":
					// Anticheat: we failed end-of-round checks. The state field picks the
					// rung — a cleared frame dismisses everything; "cooldown"/"ignored" show
					// a countdown (no test); anything else (incl. a v3-era frame) is the math
					// test gate. The three states are mutually exclusive, so each path clears
					// the others.
					var tm = Deser<TestMsg>( json );
					if ( tm.Cleared )
					{
						ClearTest();
						ClearSanction();
					}
					else if ( tm.State == "cooldown" || tm.State == "ignored" )
					{
						ClearTest();
						SanctionState = tm.State;
						SanctionUntilMs = tm.UntilMs;
						SanctionMessage = tm.Message ?? "";
					}
					else
					{
						ClearSanction();
						HasTest = true;
						TestId = tm.Id ?? "";
						TestPrompt = tm.Prompt ?? "";
						TestMessage = tm.Message ?? "";
					}
					break;

				case "achievement":
					// Out-of-band unlock the server pushed for a feat it detected off the
					// game socket (e.g. fart / hackerman), matched to us by IP.
					AchievementTracker.OnAchievement( Deser<AchievementMsg>( json ).Ident );
					break;
			}
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] bad ws frame: {e.Message}" );
		}
	}

	// Map the hello snapshot to a starting phase. Only `pending` (a round that is
	// still arming) is safe to join straight into — the client will receive that
	// round's `armed` frame, nonce and all, and can score. Every other phase means
	// we connected mid-round (armed/result) or between games (intermission): we
	// can't score the round in flight, so we sit in Waiting until the next `armed`.
	static GamePhase PhaseFrom( string s ) => s switch
	{
		"pending" => GamePhase.Pending,
		_ => GamePhase.Waiting,
	};

	static T Deser<T>( string json ) => JsonSerializer.Deserialize<T>( json, JsonOpts );
}
