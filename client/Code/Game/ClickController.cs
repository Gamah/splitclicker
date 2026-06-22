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

	/// <summary>API version to talk to, editable in the scene inspector. "v5" is the
	/// real game (the live-window tick: descending counter + opponent pips, on top of
	/// the v4 anticheat sanction ladder); "v4" is the previous live build (no tick);
	/// "v3" and below exercise the legacy/troll path the server gives clients below
	/// its live version. LEAVE BLANK to use raw, unversioned paths (bare /ws) — for
	/// the live old master backend. Applied to <see cref="ApiClient.ApiVersion"/> at
	/// startup.</summary>
	[Property] public string ApiVersion { get; set; } = "v5";

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

	/// <summary>Bumped whenever the client should re-fetch the bounty state (the
	/// skin/countdown + the previous winner): on every `hello` (so a fresh connect
	/// AND a reconnect both refresh) and on every `bounty_update` push (a rollover).
	/// The Hud watches this and reloads /config + /bounties/previous when it changes,
	/// so the client never sits in a stale post-rollover view.</summary>
	public int BountyRefreshSeq { get; private set; }

	/// <summary>Host-editable broadcast note (shown orange under the throttle line);
	/// empty = none. Set from the dev_note frame (once per game) and the hello
	/// snapshot, and only ever changed by those — it persists across rounds and
	/// reconnects until the server sends an empty note.</summary>
	public string DevNote { get; private set; } = "";

	/// <summary>Click frames actually sent to the API during the current/just-ended
	/// CLICK! phase. Reset on each arm; shown under the button.</summary>
	public int ClicksSent { get; private set; }

	/// <summary>Clicks remaining in the live window, counted down from the live tick
	/// frame (server-authoritative) so the player watches the race fill in real time.
	/// Reset to the full N on each arm; the descending counter is shown while armed.</summary>
	public int RemainingThisRound { get; private set; }

	/// <summary>Opponent click pips currently fading on screen: each a half-size
	/// button at a normalized x/y with the clicker's username, spawned from the
	/// jitter buffer at its true relative moment (see <see cref="OnData"/>). Read by
	/// the Hud, which renders + fades them. Own clicks are never added (self-dedupe).</summary>
	public List<PipButton> ActivePips { get; } = new();

	/// <summary>The live multi-button board (v5): the buttons currently clickable this
	/// armed window, each at a server-placed normalized position. Seeded from the armed
	/// frame's buttons and kept in sync by the tick claim events (claimed buttons removed,
	/// their replacements added). Empty when below-v5 (single-button via <c>_nonce</c>) or
	/// not armed. The Hud renders one clickable button per entry.</summary>
	public List<LiveButton> LiveButtons { get; } = new();

	/// <summary>True while the v5 multi-button board is the scoring surface (vs the
	/// below-v5 single persistent button driven by <c>_nonce</c>).</summary>
	public bool HasBoard => LiveButtons.Count > 0;

	/// <summary>True when this build talks v5+ (the multi-button board). In board mode
	/// the only scoring surface is the server-placed board, which exists only while
	/// armed — so the client NEVER draws its own big roaming button: during the arming
	/// wait, or for stray bad clicks, there is simply no button. Below v5 (v4/legacy/raw)
	/// the single roaming button is still the scoring + idle-click surface, so it's drawn.
	/// Derived once from the configured <see cref="ApiVersion"/> at startup.</summary>
	public bool BoardMode { get; private set; }

	/// <summary>Opponent cursors to draw this frame: each a labelled dot at a normalized
	/// position, refreshed from the tick's cursor sample and expired shortly after (so a
	/// cursor that drops out of the sample fades rather than freezing). Armed-only;
	/// cleared at the arming stage and on round/game end.</summary>
	public List<CursorDot> Cursors { get; } = new();

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

	/// <summary>True only while a valid click can score — drives the button's enabled
	/// state and scoring eligibility from one source. Armed with either a live board
	/// (v5) or a persistent legacy nonce (below-v5).</summary>
	public bool CanClick => Phase == GamePhase.Armed && ( HasBoard || !string.IsNullOrEmpty( _nonce ) );

	static readonly JsonSerializerOptions JsonOpts = new() { PropertyNameCaseInsensitive = true };

	// Idle clicks sent this round; drives the locally-counted throttle estimate.
	int _idleClicks;

	WsClient _ws;
	string _nonce;
	bool _connecting;
	int _reconnectAttempt;
	float _reconnectAt;

	// ── live-window pips (the opponent click visualization) ──
	// _roster maps a player's public tag → username, delivered in round_pending so a
	// pip (which carries only the 4-byte tag) can be labelled. _tickMs is the server
	// tick interval (sizes the jitter-buffer delay). _localArmReceive is RealTime.Now
	// when this client received `armed` — the origin each pip's t_arm offset is added
	// to, so per-client latency just shifts everything uniformly (invisible).
	readonly Dictionary<string, string> _roster = new();
	int _tickMs;
	float _localArmReceive;

	// The jitter buffer: sampled opponent clicks waiting to play at their true
	// relative moment (FireAt). Pips trail real time by PipDelay so timestamps that
	// are already in the past on arrival don't all clump at "fire now". Drains over
	// round_result; cleared at the next round_pending (the arming stage).
	readonly List<PendingPip> _pipBuffer = new();

	const float PipLifetime = 0.45f;  // how long a spawned pip button fades over
	const int PipActiveCap = 16;      // max concurrent fading buttons (drop oldest)
	const int PipBufferCap = 96;      // max buffered pending pips (drop overflow)
	const int PipMarginMs = 40;       // jitter margin added on top of one tick interval

	// One buffered opponent click awaiting its scheduled play moment.
	struct PendingPip
	{
		public float X, Y;     // normalized −1..1 click position (0 = centre)
		public string Name;    // resolved username (may be "" if unknown)
		public float FireAt;   // RealTime.Now at which to spawn it
	}

	// One opponent pip currently on screen (fading). Born drives the fade/expiry.
	public sealed class PipButton
	{
		public float X, Y;
		public string Name;
		public RealTimeSince Born;
	}

	// One live, clickable button on the v5 board. Slot is the wire handle (tick claims
	// reference it); Nonce is what a scoring click on it echoes; X,Y is its normalized
	// −1..1 position (same box-% space as the pips).
	public sealed class LiveButton
	{
		public ushort Slot;
		public string Nonce;
		public float X, Y;
	}

	// One opponent cursor to draw. Seen drives expiry when it stops being sampled.
	public sealed class CursorDot
	{
		public uint Tag;
		public float X, Y;
		public string Name;
		public RealTimeSince Seen;
	}

	const float CursorTtl = 0.3f; // drop a cursor this long after its last tick sample

	// Outbound cursor throttle: send our pointer position at most this often while armed
	// (matches the server's ~25/s ceiling; the server samples a subset into each tick).
	const float CursorSendInterval = 1f / 15f;
	float _lastCursorSent;

	// Playback delay D: at least one tick interval + a jitter margin, so the trailing
	// real-time replay never collapses to "fire now". Falls back to a small fixed
	// delay if the server didn't advertise a cadence.
	float PipDelay() => ( _tickMs > 0 ? _tickMs + PipMarginMs : 60 ) / 1000f;

	protected override void OnAwake()
	{
		Instance = this;
		if ( !string.IsNullOrWhiteSpace( BackendUrl ) )
			ApiClient.BaseUrl = BackendUrl.TrimEnd( '/' );
		// Always apply, even when blank: an empty version means raw, unversioned
		// paths (/api/… and /ws) for talking to a legacy backend like the live old
		// master (its socket is bare /ws). Skipping blank would keep the v2 default.
		ApiClient.ApiVersion = (ApiVersion ?? "").Trim().Trim( '/' );
		// Board mode = v5 or newer (the multi-button board). A blank/raw or vN<5 version
		// is the legacy single-button path that still draws the roaming button.
		BoardMode = ParseVersionMajor( ApiClient.ApiVersion ) >= 5;
		_ws = GameObject.Components.GetOrCreate<WsClient>();
		_ws.OnMessage = OnMessage;
		_ws.OnData = OnData;
		_ws.OnDone = OnDisconnected;
	}

	protected override void OnStart() => _ = ConnectFlow();

	protected override void OnUpdate()
	{
		// Jittered reconnect: only re-attempt once the backoff window elapses.
		if ( Phase == GamePhase.Disconnected && !_connecting && RealTime.Now >= _reconnectAt )
			_ = ConnectFlow();

		ProcessPips();
	}

	// Drive the pip jitter buffer: spawn any buffered pip whose scheduled moment has
	// arrived (playing its sound), and retire any on-screen pip past its fade. Run
	// every frame so replay timing is frame-accurate and fades end on time.
	void ProcessPips()
	{
		float now = RealTime.Now;
		for ( int i = _pipBuffer.Count - 1; i >= 0; i-- )
		{
			if ( now >= _pipBuffer[i].FireAt )
			{
				var p = _pipBuffer[i];
				_pipBuffer.RemoveAt( i );
				SpawnPip( p );
			}
		}
		for ( int i = ActivePips.Count - 1; i >= 0; i-- )
		{
			if ( ActivePips[i].Born >= PipLifetime )
				ActivePips.RemoveAt( i );
		}
		// Expire opponent cursors that stopped being sampled, so they fade rather than
		// freezing at a stale position.
		for ( int i = Cursors.Count - 1; i >= 0; i-- )
		{
			if ( Cursors[i].Seen >= CursorTtl )
				Cursors.RemoveAt( i );
		}
	}

	// Move a due pip onto the screen and play its blip. Caps the concurrent count by
	// dropping the oldest (same spirit as the server-side sample cap) so a burst can't
	// flood the layer or the voices.
	void SpawnPip( PendingPip p )
	{
		if ( ActivePips.Count >= PipActiveCap )
			ActivePips.RemoveAt( 0 );
		ActivePips.Add( new PipButton { X = p.X, Y = p.Y, Name = p.Name, Born = 0f } );
		SoundPlayer.PlayPip();
	}

	// OnData decodes the binary live-window `tick` frame (the only binary frame; see the
	// server's ws/tick.go for the layout):
	//   u8 opcode | u16 round | u32 remaining | u16 claimCount
	//   claimCount × ( u16 slot, u32 claimer_tag, u16 t_arm, u8 spawned,
	//                  [if spawned] u16 new_slot, u64 new_nonce, i16 x, i16 y )
	//   u8 cursorCount | cursorCount × ( u32 tag, i16 x, i16 y )
	// It updates the descending counter, applies the board mutations (removing each
	// claimed button and adding its replacement) while scheduling a pip at the claimed
	// button's position, and refreshes the opponent cursors. Own clicks/cursors are
	// skipped (self-dedupe). Runs on the socket's sync context (the main thread, same as
	// OnMessage), so it touches the buffers without locking.
	const byte TickOpcode = 1;

	void OnData( byte[] b )
	{
		try
		{
			if ( b == null || b.Length < 9 || b[0] != TickOpcode ) return;
			int round = b[1] | (b[2] << 8);
			long remaining = (uint)(b[3] | (b[4] << 8) | (b[5] << 16) | (b[6] << 24));

			// Only the current armed round's state is meaningful; a late tick from a
			// closed round must not stomp the next round's counter or board.
			bool currentRound = Phase == GamePhase.Armed && round == Round;
			if ( currentRound )
				RemainingThisRound = (int)remaining;

			int claimCount = b[7] | (b[8] << 8);
			int off = 9;
			for ( int i = 0; i < claimCount; i++ )
			{
				if ( off + 9 > b.Length ) return; // truncated frame
				ushort slot = (ushort)(b[off] | (b[off + 1] << 8));
				uint claimerTag = (uint)(b[off + 2] | (b[off + 3] << 8) | (b[off + 4] << 16) | (b[off + 5] << 24));
				ushort tArm = (ushort)(b[off + 6] | (b[off + 7] << 8));
				byte spawned = b[off + 8];
				off += 9;

				// Remove the claimed button (board stays in sync with the server) and, for
				// someone else's click in the current round, schedule a pip where it sat.
				var claimed = TakeButton( slot );
				string tagHex = Hex8( claimerTag );
				if ( currentRound && tagHex != Tag && _pipBuffer.Count < PipBufferCap )
				{
					_roster.TryGetValue( tagHex, out var name );
					_pipBuffer.Add( new PendingPip
					{
						X = claimed?.X ?? 0f,
						Y = claimed?.Y ?? 0f,
						Name = name ?? "",
						// At the click's true moment relative to our local arm receipt, trailed
						// by D so past-dated timestamps don't all fire at once.
						FireAt = _localArmReceive + tArm / 1000f + PipDelay(),
					} );
				}

				if ( spawned != 0 )
				{
					if ( off + 14 > b.Length ) return; // truncated frame
					ushort newSlot = (ushort)(b[off] | (b[off + 1] << 8));
					ulong nonce = ReadU64( b, off + 2 );
					short nx = (short)(b[off + 10] | (b[off + 11] << 8));
					short ny = (short)(b[off + 12] | (b[off + 13] << 8));
					off += 14;
					if ( currentRound )
						AddButton( newSlot, HexNonce( nonce ), nx / 32767f, ny / 32767f );
				}
			}

			// Cursor sample: refresh each named opponent cursor to this tick's position
			// (expired by ProcessPips if it stops being sampled). Skip our own.
			if ( off < b.Length )
			{
				int cursorCount = b[off];
				off += 1;
				for ( int i = 0; i < cursorCount && off + 8 <= b.Length; i++, off += 8 )
				{
					uint tag = (uint)(b[off] | (b[off + 1] << 8) | (b[off + 2] << 16) | (b[off + 3] << 24));
					short cx = (short)(b[off + 4] | (b[off + 5] << 8));
					short cy = (short)(b[off + 6] | (b[off + 7] << 8));
					if ( !currentRound || Hex8( tag ) == Tag ) continue;
					UpdateCursor( tag, cx / 32767f, cy / 32767f );
				}
			}
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] bad tick frame: {e.Message}" );
		}
	}

	// ── board + cursor helpers (touched only on the socket sync context) ──

	// TakeButton removes and returns the live button with this slot, or null if it's
	// already gone (a duplicate/late claim, or our own optimistic state).
	LiveButton TakeButton( ushort slot )
	{
		for ( int i = 0; i < LiveButtons.Count; i++ )
		{
			if ( LiveButtons[i].Slot == slot )
			{
				var btn = LiveButtons[i];
				LiveButtons.RemoveAt( i );
				return btn;
			}
		}
		return null;
	}

	void AddButton( ushort slot, string nonce, float x, float y )
	{
		LiveButtons.Add( new LiveButton { Slot = slot, Nonce = nonce, X = x, Y = y } );
	}

	// FindButton resolves the live board button currently at this slot, or null if it's
	// already been claimed/replaced. The Hud's click handler addresses buttons by slot
	// (not a captured object) so a click always scores the button live at that slot —
	// never a stale closure capture. Same socket sync context as the tick decode/the Hud
	// click, so it touches the list without locking.
	public LiveButton FindButton( ushort slot )
	{
		foreach ( var b in LiveButtons )
			if ( b.Slot == slot )
				return b;
		return null;
	}

	// Refresh (or add) an opponent cursor by tag, resetting its expiry timer.
	void UpdateCursor( uint tag, float x, float y )
	{
		for ( int i = 0; i < Cursors.Count; i++ )
		{
			if ( Cursors[i].Tag == tag )
			{
				Cursors[i].X = x;
				Cursors[i].Y = y;
				Cursors[i].Seen = 0f;
				return;
			}
		}
		_roster.TryGetValue( Hex8( tag ), out var name );
		Cursors.Add( new CursorDot { Tag = tag, X = x, Y = y, Name = name ?? "", Seen = 0f } );
	}

	// The integer major from a version segment like "v5" → 5; 0 for blank/raw or any
	// unparseable form (so blank and below-v5 both fall to the legacy single-button path).
	static int ParseVersionMajor( string ver )
	{
		if ( string.IsNullOrWhiteSpace( ver ) ) return 0;
		ver = ver.Trim().TrimStart( 'v', 'V' );
		return int.TryParse( ver, out var n ) ? n : 0;
	}

	static ulong ReadU64( byte[] b, int off )
	{
		ulong v = 0;
		for ( int i = 7; i >= 0; i-- )
			v = (v << 8) | b[off + i];
		return v;
	}

	// Lower-case hex of a 64-bit nonce. The server reads it back with ParseUint(_,16,64),
	// so leading zeros are harmless — a fixed 16-char form is simplest and locale-safe.
	static string HexNonce( ulong v )
	{
		const string h = "0123456789abcdef";
		var c = new char[16];
		for ( int i = 15; i >= 0; i--, v >>= 4 )
			c[i] = h[(int)(v & 0xF)];
		return new string( c );
	}

	// Lower-case, zero-padded 8-char hex of a 32-bit tag — matches the server's
	// PlayerTag (first 8 hex chars of a sha256). Built by hand to stay clear of any
	// culture-sensitive number formatting the sandbox doesn't whitelist.
	static string Hex8( uint v )
	{
		const string h = "0123456789abcdef";
		var c = new char[8];
		for ( int i = 7; i >= 0; i--, v >>= 4 )
			c[i] = h[(int)(v & 0xF)];
		return new string( c );
	}

	// SendClick is the hot path. While armed, fire the nonce frame (it scores) and
	// count it. In EVERY other connected phase — dormant, mid-round stand-by, result,
	// game over — send an idle click with no nonce: it scores nothing but is a "bad"
	// click the server penalises (the escalating arm-delay), so the player sees the
	// throttle they're inflicting on themselves no matter when they mash. Returns true
	// when a bad click was actually sent (so the HUD can pop a "+ PENALTY"); false for
	// a scoring click or when there's no socket to penalise it.
	public bool SendClick( float nx = 0f, float ny = 0f, bool hasPos = false )
	{
		// Scoring here is the below-v5 single-button path only: armed, no v5 board, and a
		// live legacy nonce. In v5 the board buttons score via SendButtonClick, so a bare
		// SendClick (empty space) is a miss — a penalised idle click.
		bool legacyScore = Phase == GamePhase.Armed && !HasBoard && !string.IsNullOrEmpty( _nonce );

		// Local audio feedback, independent of socket state: the "click" blip for a real
		// scoring click, the "throttle" nope otherwise. Plays even while disconnected.
		if ( legacyScore ) SoundPlayer.PlayClick();
		else SoundPlayer.PlayThrottle();

		if ( _ws == null || !_ws.Connected ) return false;
		if ( legacyScore )
		{
			// A scoring click carries the button's normalized centre (−1..1 per axis,
			// int16-scaled) so other players see it as a positioned pip. Older servers
			// ignore the extra x/y fields. Integer interpolation is locale-safe.
			if ( hasPos )
			{
				int xi = (int)Math.Clamp( nx * 32767f, -32767f, 32767f );
				int yi = (int)Math.Clamp( ny * 32767f, -32767f, 32767f );
				_ = _ws.Send( $"{{\"t\":\"click\",\"nonce\":\"{_nonce}\",\"x\":{xi},\"y\":{yi}}}" );
			}
			else
			{
				_ = _ws.Send( $"{{\"t\":\"click\",\"nonce\":\"{_nonce}\"}}" );
			}
			ClicksSent++;
			return false;
		}

		// A miss or any non-armed phase: an idle (bad) click. Send it so the server sees
		// and penalises it, even mid-round / between games.
		_ = _ws.Send( "{\"t\":\"click\",\"nonce\":\"\"}" );
		// Mirror the server's escalating bad-click penalty locally so the throttle
		// climbs the instant the player mashes; the next armed frame's authoritative
		// value overwrites this estimate (and resets the count — see OnMessage).
		_idleClicks++;
		PenaltyMs = IdlePenaltyMs( _idleClicks );
		return true;
	}

	// SendButtonClick is the v5 hot path: a click that landed on board button `btn`. It
	// echoes that button's nonce (scores) plus the button's position (for the opponent
	// pip), plays the click blip, and counts it. It deliberately does NOT remove the
	// button locally — the authoritative tick claim does that, so a lost race just
	// resolves when the claim shows someone else took it. A null button or a non-armed
	// phase falls through to the penalised idle path.
	public bool SendButtonClick( LiveButton btn )
	{
		if ( btn == null || Phase != GamePhase.Armed )
			return SendClick( btn?.X ?? 0f, btn?.Y ?? 0f, btn != null );

		SoundPlayer.PlayClick();
		if ( _ws == null || !_ws.Connected ) return false;
		int xi = (int)Math.Clamp( btn.X * 32767f, -32767f, 32767f );
		int yi = (int)Math.Clamp( btn.Y * 32767f, -32767f, 32767f );
		_ = _ws.Send( $"{{\"t\":\"click\",\"nonce\":\"{btn.Nonce}\",\"x\":{xi},\"y\":{yi}}}" );
		ClicksSent++;
		return false;
	}

	// SendCursor reports the local pointer to the server while armed (so others see our
	// roaming cursor), throttled to CursorSendInterval. The Hud calls it each frame with
	// the pointer in the same −1..1 box space the buttons/pips use. A no-op off-armed —
	// cursors are an armed-window thing only.
	public void SendCursor( float nx, float ny )
	{
		if ( Phase != GamePhase.Armed || _ws == null || !_ws.Connected ) return;
		if ( RealTime.Now - _lastCursorSent < CursorSendInterval ) return;
		_lastCursorSent = RealTime.Now;
		int xi = (int)Math.Clamp( nx * 32767f, -32767f, 32767f );
		int yi = (int)Math.Clamp( ny * 32767f, -32767f, 32767f );
		_ = _ws.Send( $"{{\"t\":\"cursor\",\"x\":{xi},\"y\":{yi}}}" );
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
			// Show the resolved display name; persist ONLY a genuinely-claimed
			// handle. Saving the display-name fallback here is what caused the
			// Steam name to be re-sent as a username and 422 on every reconnect.
			Username = string.IsNullOrEmpty( auth.DisplayName ) ? auth.Username : auth.DisplayName;
			pd.Username = auth.Username ?? "";
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
		LiveButtons.Clear();
		Cursors.Clear();
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
					_tickMs = h.Game.TickMs;
					DevNote = h.Game.DevNote ?? "";
					Phase = PhaseFrom( h.Game.Phase );
					// A (re)connect: refresh the bounty state so a client that was offline
					// across a rollover picks up the new skin + previous winner on rejoin.
					BountyRefreshSeq++;
					break;

				case "round_pending":
					var p = Deser<PendingMsg>( json );
					Round = p.Round;
					Of = p.Of;
					Players = p.Players;
					ClicksToWin = p.Clicks;
					// Refresh the tag→username roster for this round's pips, and clear the
					// pip jitter buffer + any on-screen pips at the arming stage: there are
					// several seconds here, so the previous round's tail has long since
					// drained over its result, and this guarantees no stale pip survives into
					// the next live window (the deliberate "clear at arming, not at armed").
					_roster.Clear();
					if ( p.Roster != null )
					{
						foreach ( var e in p.Roster )
							if ( !string.IsNullOrEmpty( e.Tag ) ) _roster[e.Tag] = e.Username ?? "";
					}
					_pipBuffer.Clear();
					ActivePips.Clear();
					// The live board + opponent cursors belong to a window; drop any held from
					// the last one so nothing stale shows through this arming gap.
					LiveButtons.Clear();
					Cursors.Clear();
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
					RemainingThisRound = a.Clicks; // start the live counter at full N; ticks count it down
					_localArmReceive = RealTime.Now; // origin for every pip's t_arm replay offset
					_lastCursorSent = 0f;          // allow the first cursor send immediately
					// v5: seed the live board from the armed buttons; tick claims then keep it
					// in sync. Below-v5 servers send no buttons and a single persistent nonce.
					LiveButtons.Clear();
					Cursors.Clear();
					if ( a.Buttons != null )
					{
						foreach ( var bt in a.Buttons )
							if ( !string.IsNullOrEmpty( bt.Nonce ) )
								AddButton( (ushort)bt.Id, bt.Nonce, bt.X / 32767f, bt.Y / 32767f );
					}
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
					// Round closed: the remaining (now meaningless) buttons + cursors disappear.
					LiveButtons.Clear();
					Cursors.Clear();
					SoundPlayer.PlayDisarm();
					AchievementTracker.OnRoundResult( r.You.PointsDelta, r.You.RoundId );
					break;

				case "game_over":
					var g = Deser<GameOverMsg>( json );
					Standings = g.Standings ?? new();
					Phase = GamePhase.GameOver;
					_nonce = null;
					LiveButtons.Clear();
					Cursors.Clear();
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
					// a countdown (no test); anything else (incl. an empty state) is the math
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

				case "bounty_update":
					// The active bounty rolled over: nudge the Hud to re-fetch /config +
					// /bounties/previous so the new skin/countdown and the just-settled
					// winner appear immediately (no payload — the HTTP endpoints are truth).
					BountyRefreshSeq++;
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
