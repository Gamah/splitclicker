using System;
using System.Linq;
using Sandbox;
using Skafinity;
using Splitclicker.Game;

namespace Splitclicker.Audio;

/// <summary>
/// Game glue around the <see cref="Skafinity.SkafinityPlayer"/> library component (the
/// procedural ska / reggae-rock engine — <see cref="Skafinity.MusicGen"/>). Per-client
/// singleton the music panel talks to; not networked (splitclicker has no lobby).
///
/// All generator tuning lives in the Skafinity library; this class adds only the
/// game-specific layer the library deliberately leaves out:
///  • a per-client <see cref="Instance"/> singleton the UI talks to,
///  • <see cref="PlayerData"/>-backed persistence of the seed (tag / vibe / song index) and
///    the mute + volume choices,
///  • the fixed splitclicker master volume the user's 0–1.5 control multiplies.
///
/// The library player is created at runtime on this GameObject and driven from here — its
/// own inspector knobs keep the Skafinity defaults (the canonical tuning). This is the
/// rotaliate MusicController stripped of its admin-follow / networking layer.
/// </summary>
public sealed class MusicController : Component
{
	public static MusicController Instance { get; private set; }

	/// <summary>Fixed splitclicker master the user's 0–1.5 <see cref="PlayerData.MusicVolume"/>
	/// multiplies (the music sits low under the game, as the original inline controller did).</summary>
	const float BaselineVolume = 0.15f;

	SkafinityPlayer _player;
	bool _needStart;          // deferred initial start (runs once the player component exists)
	int _lastPersistedN;      // last song index written back to PlayerData

	// ── Read surface the UI consumes ──

	/// <summary>Currently-playing song index.</summary>
	public int N => _player?.N ?? Math.Max( 0, PlayerData.Load()?.MusicN ?? 0 );
	/// <summary>The effective vibe string (knobs with any override applied).</summary>
	public string CurrentVibe => _player?.CurrentVibe ?? "";
	/// <summary>Shareable seed for the playing song: <c>vibe:tag:n</c>.</summary>
	public string CurrentSeed => _player?.CurrentSeed ?? "—";
	/// <summary>Raw override tag ("" = your own) for the panel's text-entry default.</summary>
	public string TagField => PlayerData.Load()?.MusicTag ?? "";
	/// <summary>The config currently in effect — the source the vibe matrix reads positions from.</summary>
	public MusicGen.Config EffectiveConfig() => _player?.EffectiveConfig() ?? new MusicGen.Config();
	/// <summary>Genre index currently in effect (rides in the vibe).</summary>
	public int Genre => EffectiveConfig().Genre;

	/// <summary>Player's persisted mute choice; silences the track without stopping it.</summary>
	public bool Muted => PlayerData.Load()?.MusicMuted ?? false;
	/// <summary>Player's persisted volume multiplier (0–1.5; 1 = baseline).</summary>
	public float Volume => PlayerData.Load()?.MusicVolume ?? 1f;

	// The player tag seeds the song; a saved MusicTag overrides it (empty = own tag).
	string SeedTag
	{
		get
		{
			var d = PlayerData.Load();
			var t = d?.MusicTag;
			return string.IsNullOrEmpty( t ) ? (d?.PlayerTag ?? "") : t;
		}
	}

	protected override void OnStart()
	{
		Instance = this;

		// Adopt the one library player for the whole scene. If one was placed in the scene (or
		// added while trying the library), reuse it rather than spawning a second — two players
		// = two overlapping streams. Mute any extras so only the one we drive is audible.
		var existing = Scene.GetAllComponents<SkafinityPlayer>().ToList();
		_player = existing.FirstOrDefault() ?? GameObject.Components.Create<SkafinityPlayer>();
		foreach ( var p in existing )
			if ( p != _player ) { p.AutoPlay = false; p.Enabled = false; }

		// We own the seed + persistence, so the player never auto-plays or self-persists.
		_player.AutoPlay = false;
		_player.PersistProgress = false;
		_player.LiveReload = false;
		_player.MixerName = "Music";
		_lastPersistedN = Math.Max( 0, PlayerData.Load()?.MusicN ?? 0 );
		// Seed the player's own start index too, so if its OnStart runs after ours it lands on
		// the right song rather than resetting to 0.
		_player.StartN = _lastPersistedN;
		ApplyUserVolume();

		_needStart = true;   // start once the player component has finished its own OnStart
	}

	protected override void OnDestroy()
	{
		if ( Instance == this ) Instance = null;
	}

	protected override void OnUpdate()
	{
		if ( _player == null ) return;

		// Deferred initial start: kicks the player at the player's own persisted seed.
		if ( _needStart )
		{
			_needStart = false;
			ApplyOwnSeed();
		}

		ApplyUserVolume();

		// Persist the auto-advancing song index back to PlayerData.
		if ( _player.N != _lastPersistedN )
		{
			_lastPersistedN = _player.N;
			var d = PlayerData.Load() ?? new PlayerData();
			d.MusicN = _player.N;
			d.Save();

			// Shuffle mode: each new song gets a freshly randomized set of knobs.
			if ( RandomEverySong ) RerollAll();
		}
	}

	void ApplyUserVolume()
	{
		var d = PlayerData.Load();
		_player.Enabled = !(d?.MusicMuted ?? false);
		_player.Volume = BaselineVolume * (d?.MusicVolume ?? 1f);
	}

	// Point the player at the local player's own saved seed (tag / vibe / n) and (re)start.
	void ApplyOwnSeed()
	{
		var d = PlayerData.Load();
		_player.Tag = SeedTag;
		_player.Vibe = d?.MusicVibe ?? "";
		_lastPersistedN = Math.Max( 0, d?.MusicN ?? 0 );
		_player.SetN( _lastPersistedN );   // sets the index and restarts the sequence
	}

	// ── Mute / volume ──

	/// <summary>Flip the persisted mute state. <see cref="ApplyUserVolume"/> applies it next frame.</summary>
	public void ToggleMute()
	{
		var data = PlayerData.Load() ?? new PlayerData();
		data.MusicMuted = !data.MusicMuted;
		data.Save();
	}

	/// <summary>Set the persisted volume multiplier (clamped 0–1.5). Applied next frame.</summary>
	public void SetVolume( float v )
	{
		var data = PlayerData.Load() ?? new PlayerData();
		data.MusicVolume = Math.Clamp( v, 0f, 1.5f );
		data.Save();
	}

	// ── Mutations (persist to PlayerData, then drive the player) ──

	// Parse a shareable seed in any of vibe:tag:n / tag:n / tag. Missing parts stay null.
	static void ParseSeed( string seed, out string vibe, out string tag, out int? n )
	{
		vibe = null; tag = null; n = null;
		seed = seed?.Trim();
		if ( string.IsNullOrEmpty( seed ) ) return;
		var p = seed.Split( ':' );
		if ( p.Length >= 3 ) { vibe = p[0]; tag = p[1]; if ( int.TryParse( p[2], out var v ) ) n = v; }
		else if ( p.Length == 2 )
		{
			if ( int.TryParse( p[1], out var v ) ) { tag = p[0]; n = v; }
			else if ( VibeCodec.LooksLikeVibe( p[0] ) ) { vibe = p[0]; tag = p[1]; }
			else tag = p[0];
		}
		else tag = p[0];
	}

	/// <summary>Play a pasted shareable seed (<c>vibe:tag:n</c>, <c>tag:n</c>, or <c>tag</c>).
	/// Missing components are left unchanged; a vibe is applied only when present. Persists; restarts.</summary>
	public void PlaySeed( string seed )
	{
		if ( _player == null ) return;
		ParseSeed( seed, out string vibe, out string tag, out int? n );

		var own = (PlayerData.Load()?.PlayerTag ?? "").ToLowerInvariant();
		var data = PlayerData.Load() ?? new PlayerData();
		tag = tag?.Trim().ToLowerInvariant();
		data.MusicTag = (string.IsNullOrEmpty( tag ) || tag == own) ? "" : tag;
		if ( vibe != null ) data.MusicVibe = VibeCodec.LooksLikeVibe( vibe ) ? vibe.ToLowerInvariant() : "";
		if ( n.HasValue ) data.MusicN = Math.Max( 0, n.Value );
		data.Save();
		ApplyOwnSeed();
	}

	/// <summary>Set just the seed tag (empty/own tag = back to your own). Persists; restarts.</summary>
	public void SetTag( string tag ) => PlaySeed( tag );

	/// <summary>Set vibe field <paramref name="index"/> (see <see cref="VibeCodec.Fields(int)"/>)
	/// from a 0..1 fraction; the library re-encodes + restarts on a short debounce. Persists.</summary>
	public void SetVibe( int index, float norm )
	{
		if ( _player == null ) return;
		_player.SetVibe( index, norm );
		PersistVibe();
	}

	/// <summary>Switch genre (rides in the vibe's first char). Re-encodes + persists + restarts.</summary>
	public void SetGenre( int genre )
	{
		if ( _player == null ) return;
		var cfg = _player.EffectiveConfig();
		cfg.Genre = Math.Clamp( genre, 0, VibeCodec.GenreCount - 1 );
		_player.Vibe = VibeCodec.Encode( cfg );
		PersistVibe();
		_player.StartSequence();
	}

	/// <summary>Randomize every vibe knob except the per-instrument volumes. Persists; restarts.</summary>
	public void RerollVibe()
	{
		if ( _player == null ) return;
		_player.RerollVibe();
		PersistVibe();
	}

	/// <summary>Randomize <em>every</em> knob — global, genre, instruments, and volumes
	/// (full shuffle). Persists; restarts.</summary>
	public void RerollAll()
	{
		if ( _player == null ) return;
		_player.RerollVibe( includeVolumes: true, includeGenre: true );
		PersistVibe();
	}

	/// <summary>Shuffle mode: re-randomize every knob when each new song begins (persisted).</summary>
	public bool RandomEverySong => PlayerData.Load()?.MusicRandomEverySong ?? false;

	/// <summary>Toggle shuffle mode. Turning it on immediately rerolls the current song.</summary>
	public void SetRandomEverySong( bool on )
	{
		var data = PlayerData.Load() ?? new PlayerData();
		data.MusicRandomEverySong = on;
		data.Save();
		if ( on ) RerollAll();
	}

	void PersistVibe()
	{
		var data = PlayerData.Load() ?? new PlayerData();
		data.MusicVibe = _player.Vibe ?? "";
		data.Save();
	}

	/// <summary>Back to your own tag and your own (default-knob) vibe. Persists; restarts.</summary>
	public void UseOwn()
	{
		var data = PlayerData.Load() ?? new PlayerData();
		data.MusicTag = "";
		data.MusicVibe = "";
		data.Save();
		ApplyOwnSeed();
	}

	/// <summary>Jump to song index n (clamped ≥ 0). Persists; restarts.</summary>
	public void SetN( int n )
	{
		if ( _player == null ) return;
		var data = PlayerData.Load() ?? new PlayerData();
		data.MusicN = Math.Max( 0, n );
		data.Save();
		_lastPersistedN = data.MusicN;
		_player.SetN( data.MusicN );
	}

	public void StepN( int delta ) => SetN( N + delta );

	/// <summary>Write the playing song's raw loop (no fade) to a WAV under FileSystem.Data.</summary>
	public string SaveCurrentToFile() => _player?.SaveCurrentToFile();
}
