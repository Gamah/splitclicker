using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Threading.Tasks;
using Sandbox;
using Splitclicker.Api;
using Splitclicker.Ws;

namespace Splitclicker.Game;

public enum GamePhase
{
	Connecting,
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
	public int MyPoints { get; private set; }
	public List<Standing> Standings { get; private set; } = new();
	public List<Standing> Winners { get; private set; } = new();

	/// <summary>Connected players (open server connections) and the scoring slots
	/// this round (N = a multiple of the player count). Shown pre-click.</summary>
	public int Players { get; private set; }
	public int ClicksToWin { get; private set; }
	/// <summary>This connection's current arm-delay penalty in ms (the spam
	/// deterrent), 0 for honest clients. Surfaced so the player sees the throttle.</summary>
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

	// SendClick is the hot path: do nothing but fire the nonce frame. An honest
	// client only clicks while armed, so it never incurs the idle-spam penalty.
	public void SendClick()
	{
		if ( !CanClick ) return;
		_ = _ws.Send( $"{{\"t\":\"click\",\"nonce\":\"{_nonce}\"}}" );
		ClicksSent++;
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
					Phase = PhaseFrom( h.Game.Phase );
					break;

				case "round_pending":
					var p = Deser<PendingMsg>( json );
					Round = p.Round;
					Of = p.Of;
					Players = p.Players;
					ClicksToWin = p.Clicks;
					PenaltyMs = 0; // fresh round: throttle is recomputed and re-revealed on arm
					Phase = GamePhase.Pending;
					_nonce = null;
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
					break;

				case "round_result":
					var r = Deser<RoundResultMsg>( json );
					Round = r.Round;
					Of = r.Of;
					Winners = r.Winners ?? new();
					Standings = r.Standings ?? new();
					Phase = GamePhase.Result;
					_nonce = null;
					UpdateMyPoints();
					AchievementTracker.OnRoundResult( r.You.PointsDelta, r.You.RoundId );
					break;

				case "game_over":
					var g = Deser<GameOverMsg>( json );
					Standings = g.Standings ?? new();
					Phase = GamePhase.GameOver;
					_nonce = null;
					UpdateMyPoints();
					AchievementTracker.OnGameOver( g.You.Placement, g.You.Won, g.You.GameId );
					break;
			}
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] bad ws frame: {e.Message}" );
		}
	}

	void UpdateMyPoints()
	{
		MyPoints = 0;
		foreach ( var s in Standings )
		{
			if ( s.Tag == Tag )
			{
				MyPoints = s.Points;
				return;
			}
		}
	}

	static GamePhase PhaseFrom( string s ) => s switch
	{
		"pending" => GamePhase.Pending,
		"armed" => GamePhase.Armed,
		"result" => GamePhase.Result,
		_ => GamePhase.Connecting,
	};

	static T Deser<T>( string json ) => JsonSerializer.Deserialize<T>( json, JsonOpts );
}
