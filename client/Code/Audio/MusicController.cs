using System;
using System.Collections.Generic;
using System.Threading.Tasks;
using Sandbox;
using Splitclicker.Game;

namespace Splitclicker.Audio;

/// <summary>
/// Plays the procedural ska / reggae-rock track (see <see cref="MusicGen"/>) for the
/// local player and loops/crossfades through an endless sequence of songs.
///
/// Ported from rotaliate's MusicController, stripped to splitclicker's needs: no UI,
/// no inspector knobs, no admin-follow/networking. The generator runs from its default
/// <see cref="MusicGen.Config"/> with a randomised "vibe" rerolled once per load, the
/// volume is fixed, and the song index N is persisted to local data so the sequence
/// resumes where it left off.
/// </summary>
public sealed class MusicController : Component
{
	public static MusicController Instance { get; private set; }

	// Fixed master volume (no in-game control).
	const float Volume = 0.15f;

	// ── Output / playback tuning (rotaliate's defaults; not exposed) ──
	const int RenderThreads = 6;   // worker threads the pitched-voice synthesis fans out across
	const float Crossfade = 3.75f; // crossfade window (also the first song's fade-in), seconds
	const float CrossfadeOverlap = 0.5f; // fraction of the window the two songs both sound
	const int LoopsPerSong = 2;    // passes of each song's loop before crossfading on
	const int AheadCount = 5;      // upcoming songs kept pre-generated

	SoundStream _stream;
	SoundHandle _handle;
	int _sr;

	string _vibe = "";             // randomised once on load; applied over the default config
	short[] _curRaw;               // current song, full single loop (raw)
	readonly List<short[]> _ahead = new(); // pre-generated songs n+1, n+2, …
	int _curN;                     // index of the currently-playing song
	int _curReserve;               // samples of the current song's tail held for the crossfade
	double _pushedSeconds;         // total audio pushed to the stream
	TimeSince _sinceStart;         // wall clock since playback started
	bool _starting;                // StartSequenceAsync is in flight
	bool _fillingAhead;            // FillAhead is in flight
	bool Generating => _starting || _fillingAhead;
	int _seq;                      // bumped on each StartSequence; stale async results are discarded
	bool _flatConfigured;          // ConfigureFlat applied to the live handle

	// The PRNG seed tag: the player's tag keeps each player's sequence its own; the song
	// index n walks the sequence. (Empty tag falls back to a fixed string.)
	string SeedTag
	{
		get
		{
			var t = PlayerData.Load()?.PlayerTag;
			return string.IsNullOrEmpty( t ) ? "splitclicker" : t.ToLowerInvariant();
		}
	}

	static string SeedFor( string tag, int n ) => $"{( string.IsNullOrEmpty( tag ) ? "splitclicker" : tag.ToLowerInvariant() )}:{n}";
	string Seed( int n ) => SeedFor( SeedTag, n );

	protected override void OnStart()
	{
		Instance = this;
		RerollVibe();                                       // fresh vibe each load
		_curN = Math.Max( 0, PlayerData.Load()?.MusicN ?? 0 ); // resume the saved song index
		StartSequence();
	}

	protected override void OnDestroy()
	{
		if ( Instance == this ) Instance = null;
		_seq++;            // invalidate any in-flight worker generation
		_handle?.Stop();
		_handle = null;
		_stream = null;
	}

	protected override void OnUpdate()
	{
		if ( _handle != null )
			_handle.Volume = Volume;

		// Keep the look-ahead buffer topped up. Generation runs on a worker thread
		// (FillAhead), so this never blocks the frame.
		if ( !Generating && _curRaw != null && _ahead.Count < Math.Max( 1, AheadCount ) )
			_ = FillAhead( _seq );

		// When the queued audio is about to run out, crossfade into the next song.
		if ( _stream != null && _ahead.Count > 0 && _curRaw != null
			&& _pushedSeconds - _sinceStart < 2.0 )
			PushTransition();
	}

	// ── vibe ──

	// Per-instrument volumes are left at their defaults by the reroll.
	static readonly HashSet<string> VolumeFields =
		new() { "BASS", "SKANK", "ORGAN", "LEAD", "HORNS", "DRUMS" };

	/// <summary>Randomise every vibe knob except the per-instrument volumes, into the
	/// in-memory vibe used for this session's generation. Not persisted (rerolled each load).</summary>
	void RerollVibe()
	{
		var cfg = new MusicGen.Config();
		var rng = System.Random.Shared;
		foreach ( var f in VibeCodec.Fields )
		{
			if ( VolumeFields.Contains( f.Name ) ) continue;
			f.SetNorm( cfg, rng.NextSingle() );
		}
		if ( cfg.BpmMin > cfg.BpmMax ) (cfg.BpmMin, cfg.BpmMax) = (cfg.BpmMax, cfg.BpmMin);
		_vibe = VibeCodec.Encode( cfg );
	}

	// The config the generator runs from: defaults with this session's vibe applied.
	MusicGen.Config BuildConfig()
	{
		var cfg = new MusicGen.Config();
		if ( !string.IsNullOrEmpty( _vibe ) )
			VibeCodec.Apply( _vibe, cfg );
		return cfg;
	}

	// ── synthesis (off the main thread) ──

	// Run the (pure, CPU-heavy) synthesis on worker threads so it never blocks the frame
	// AND so no single worker burst trips s&box's ~1000ms no-yield advisory. Composition
	// + drums are RNG-bound and stay sequential; the pitched voices pull no RNG, so they
	// fan out across RenderThreads disjoint windows joined by Task.WhenAll.
	async Task<short[]> GenerateMonoAsync( string seedStr, MusicGen.Config cfg )
	{
		MusicGen g = null;
		await GameTask.RunInThreadAsync( () => { g = MusicGen.BeginPlan( seedStr, cfg ); return Task.CompletedTask; } );

		int total = g.TotalSamples;
		int k = Math.Clamp( RenderThreads, 1, 8 );
		if ( k <= 1 )
		{
			await GameTask.RunInThreadAsync( () => { g.RenderPitchedRange( 0, total ); return Task.CompletedTask; } );
		}
		else
		{
			var jobs = new Task[k];
			for ( int i = 0; i < k; i++ )
			{
				int from = (int)((long)total * i / k);
				int to = (int)((long)total * (i + 1) / k);
				jobs[i] = GameTask.RunInThreadAsync( () => { g.RenderPitchedRange( from, to ); return Task.CompletedTask; } );
			}
			await Task.WhenAll( jobs );
		}

		short[] mono = null;
		await GameTask.RunInThreadAsync( () => { mono = g.FinishMono(); return Task.CompletedTask; } );
		_sr = g.SampleRate;
		return mono;
	}

	int FadeSamples => Math.Max( 1, (int)(Math.Clamp( Crossfade, 0.25f, 8f ) * _sr) );

	/// <summary>Top the look-ahead buffer up to <see cref="AheadCount"/>, generating each
	/// song on a worker thread. Fire-and-forget from OnUpdate; <paramref name="seq"/> guards
	/// against a sequence restart landing a stale song in the buffer.</summary>
	async Task FillAhead( int seq )
	{
		if ( Generating ) return;
		try
		{
			_fillingAhead = true;
			string tag = SeedTag;
			var cfg = BuildConfig();
			while ( seq == _seq && _curRaw != null && _ahead.Count < Math.Max( 1, AheadCount ) )
			{
				int n = _curN + 1 + _ahead.Count;
				var song = await GenerateMonoAsync( SeedFor( tag, n ), cfg );
				if ( seq != _seq ) return;   // sequence restarted while we were generating
				_ahead.Add( song );
			}
		}
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] MusicController.FillAhead failed: {e.Message}" ); }
		finally { _fillingAhead = false; }
	}

	/// <summary>Write <see cref="LoopsPerSong"/> passes of <paramref name="raw"/> to the
	/// stream, given the first <paramref name="headConsumed"/> samples of pass 0 were already
	/// emitted, holding back the final <paramref name="reserve"/> samples for the next
	/// crossfade. Optional fade-in over the first <paramref name="fadeIn"/> samples.</summary>
	int WriteSongBody( short[] raw, int headConsumed, int reserve, int fadeIn )
	{
		int loops = Math.Max( 1, LoopsPerSong );
		int total = 0;
		for ( int loop = 0; loop < loops; loop++ )
		{
			int start = loop == 0 ? headConsumed : 0;
			int end = loop == loops - 1 ? raw.Length - reserve : raw.Length;
			if ( end <= start ) continue;
			int len = end - start;
			var seg = new short[len];
			for ( int i = 0; i < len; i++ )
			{
				int idx = start + i;
				float g = (loop == 0 && fadeIn > 0 && idx < fadeIn) ? (float)idx / fadeIn : 1f;
				seg[i] = (short)(raw[idx] * g);
			}
			_stream.WriteData( seg );
			total += len;
		}
		return total;
	}

	/// <summary>(Re)start the infinite sequence at the current tag/n. Bumps the sequence
	/// token (invalidating any in-flight generation), stops the current handle, then kicks
	/// the async (worker-thread) start so the caller never blocks.</summary>
	void StartSequence()
	{
		int seq = ++_seq;
		_ahead.Clear();
		_handle?.Stop();
		_handle = null;
		_stream = null;
		_flatConfigured = false;
		_curRaw = null;
		_ = StartSequenceAsync( seq );
	}

	// The first song fades in; thereafter songs play LoopsPerSong passes and crossfade into
	// the pre-generated next. Synthesis is offloaded; stream setup is on the main thread.
	async Task StartSequenceAsync( int seq )
	{
		try
		{
			_starting = true;
			int n = _curN;
			string tag = SeedTag;
			var cfg = BuildConfig();
			var raw = await GenerateMonoAsync( SeedFor( tag, n ), cfg );
			if ( seq != _seq ) return;   // superseded by a newer StartSequence

			_curN = n;
			_curRaw = raw;
			int fade = Math.Min( FadeSamples, _curRaw.Length / 3 );
			_curReserve = fade;

			_stream = new SoundStream( _sr );

			int written = WriteSongBody( _curRaw, 0, _curReserve, fade );
			_pushedSeconds = written / (double)_sr;

			_handle = _stream.Play();
			if ( _handle != null )
			{
				_handle.Volume = Volume;
				ConfigureFlat();
			}
			_sinceStart = 0;

			SaveN(); // persist the resumed song index

			Log.Info( $"[Splitclicker] MusicController: start seed='{Seed( _curN )}' sr={_sr} loop={_curRaw.Length / (float)_sr:0.0}s handle={( _handle == null ? "NULL" : "ok" )}" );
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] MusicController.StartSequence failed: {e.Message}" );
		}
		finally { _starting = false; }
	}

	// Queue the crossfade from the current song's tail into the next song's head, then the
	// next song's body. Advances n and persists it.
	void PushTransition()
	{
		try
		{
			var next = _ahead[0];
			_ahead.RemoveAt( 0 );

			// Crossfade window = the current song's held-back tail. The two songs only
			// overlap for CrossfadeOverlap of this window, centred — the rest plays clear.
			int W = Math.Min( _curReserve, next.Length / 3 );
			int curStart = _curRaw.Length - W;
			int cross = Math.Clamp( (int)(W * CrossfadeOverlap), 1, W );
			int ws = (W - cross) / 2;     // overlap starts here
			int we = ws + cross;          // overlap ends here

			var xf = new short[W];
			for ( int i = 0; i < W; i++ )
			{
				float gOut, gIn;
				if ( i < ws ) { gOut = 1f; gIn = 0f; }            // outgoing in the clear
				else if ( i >= we ) { gOut = 0f; gIn = 1f; }      // incoming in the clear
				else
				{
					double t = (i - ws + 0.5) / cross * (Math.PI / 2); // equal-power cross
					gOut = (float)Math.Cos( t );
					gIn = (float)Math.Sin( t );
				}
				xf[i] = (short)Math.Clamp( _curRaw[curStart + i] * gOut + next[i] * gIn, -32768, 32767 );
			}
			_stream.WriteData( xf );

			int nextReserve = Math.Min( FadeSamples, next.Length / 3 );
			int written = WriteSongBody( next, W, nextReserve, 0 );
			_pushedSeconds += (W + written) / (double)_sr;

			_curRaw = next;
			_curReserve = nextReserve;
			_curN++;
			SaveN();
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] MusicController.PushTransition failed: {e.Message}" );
		}
	}

	// Persist the current song index so the sequence resumes here next load.
	void SaveN()
	{
		var data = PlayerData.Load() ?? new PlayerData();
		if ( data.MusicN == _curN ) return;
		data.MusicN = _curN;
		data.Save();
	}

	/// <summary>Make the stream play as flat 2D: SpacialBlend=0, parented to the camera with
	/// FollowParent so the listener can't pan/attenuate it. Routes to a "Music" mixer if one
	/// exists (else the default). Applied once per handle.</summary>
	void ConfigureFlat()
	{
		if ( _handle == null || _flatConfigured ) return;
		_handle.SpacialBlend = 0f;
		var camGo = Scene?.Camera?.GameObject;
		if ( camGo.IsValid() )
		{
			_handle.Parent = camGo;
			_handle.FollowParent = true;
		}
		var music = Sandbox.Audio.Mixer.FindMixerByName( "Music" );
		if ( music != null )
			_handle.TargetMixer = music;
		_flatConfigured = true;
	}
}
