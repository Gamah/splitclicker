using System;
using System.Text.Json;
using Sandbox;

namespace Splitclicker.Game;

// Local identity cache + achievement dedupe state, persisted to FileSystem.Data.
// Identity itself is the Steam account (resolved server-side); this just caches
// the display name/tag and the last-applied round/game ids so a reconnect replay
// can't double-count non-idempotent stat increments (PLAN §7.1).
public sealed class PlayerData
{
	public string Username { get; set; } = "";
	public string PlayerTag { get; set; } = "";

	/// <summary>round_id whose points_delta we last applied to the `points` stat.</summary>
	public string LastPointsRoundId { get; set; } = "";
	/// <summary>game_id whose win we last counted toward the `wins` stat.</summary>
	public string LastWinGameId { get; set; } = "";

	/// <summary>Index of the procedural music track currently playing, persisted so the
	/// endless song sequence resumes where it left off (see Audio/MusicController).</summary>
	public int MusicN { get; set; }

	/// <summary>Whether the player has muted the background music (persisted choice).</summary>
	public bool MusicMuted { get; set; }

	const string FileName = "player.json";
	static PlayerData _cache;

	public static PlayerData Load()
	{
		if ( _cache != null ) return _cache;
		try
		{
			if ( FileSystem.Data.FileExists( FileName ) )
			{
				_cache = JsonSerializer.Deserialize<PlayerData>( FileSystem.Data.ReadAllText( FileName ) );
			}
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] PlayerData load failed: {e.Message}" );
		}
		return _cache ??= new PlayerData();
	}

	public void Save()
	{
		_cache = this;
		try { FileSystem.Data.WriteAllText( FileName, JsonSerializer.Serialize( this ) ); }
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] PlayerData save failed: {e.Message}" ); }
	}
}
