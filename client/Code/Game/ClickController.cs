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

	/// <summary>Click frames actually sent to the API during the current/just-ended
	/// CLICK! phase. Reset on each arm; shown under the button.</summary>
	public int ClicksSent { get; private set; }

	/// <summary>The arming-window bounds (seconds) from the server config; the
	/// per-round delay itself stays secret. Shown while the round is arming.</summary>
	public int ArmMinSec { get; private set; }
	public int ArmMaxSec { get; private set; }

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
	// count it. While the button is dormant (Pending), send an idle click with no
	// nonce: it scores nothing but earns the escalating arm-delay penalty — sent so
	// the player actually sees the throttle they're inflicting on themselves. Other
	// phases send nothing.
	public void SendClick()
	{
		// Local audio feedback, independent of socket state and mirroring exactly what the
		// button shows: the "click" blip only while the race is live (the CLICK! window),
		// the "throttle" nope in every other state. Plays even while disconnected.
		if ( CanClick ) SoundPlayer.PlayClick();
		else SoundPlayer.PlayThrottle();

		if ( _ws == null || !_ws.Connected ) return;
		if ( Phase == GamePhase.Armed && !string.IsNullOrEmpty( _nonce ) )
		{
			_ = _ws.Send( $"{{\"t\":\"click\",\"nonce\":\"{_nonce}\"}}" );
			ClicksSent++;
		}
		else if ( Phase == GamePhase.Pending )
		{
			_ = _ws.Send( "{\"t\":\"click\",\"nonce\":\"\"}" );
			// Mirror the server's escalating idle-click penalty locally so the throttle
			// climbs the instant the player mashes; the armed frame's authoritative
			// value overwrites this estimate.
			_idleClicks++;
			PenaltyMs = IdlePenaltyMs( _idleClicks );
		}
	}

	// Mirror of the server's idle-click penalty (game.idlePenalty): the Nth click
	// adds N×5ms, so totals run 5,15,30,50,75,105… ms.
	const int PenaltyStepMs = 5;
	static int IdlePenaltyMs( int n ) => n <= 0 ? 0 : PenaltyStepMs * n * ( n + 1 ) / 2;

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
					Phase = PhaseFrom( h.Game.Phase );
					break;

				case "round_pending":
					var p = Deser<PendingMsg>( json );
					Round = p.Round;
					Of = p.Of;
					Players = p.Players;
					ClicksToWin = p.Clicks;
					PenaltyMs = 0; // fresh round: throttle resets, then counts up as the player idle-clicks
					_idleClicks = 0;
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
					ClicksSent = 0; // fresh CLICK! phase: start the sent tally over
					_nonce = a.Nonce;
					Phase = GamePhase.Armed;
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
