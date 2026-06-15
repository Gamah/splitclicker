using System;
using Sandbox;

namespace Splitclicker.Game;

// Maps server-pushed `you.*` facts to s&box Services (Stats + Achievements).
// The server is the source of truth; we just record. Stat increments are NOT
// idempotent, so guard them with the round_id/game_id the server stamps; manual
// Unlock calls are idempotent and need no guard (PLAN §7.1).
//
// Named *Tracker to avoid colliding with Sandbox.Services.Achievements.
public static class AchievementTracker
{
	// round_result → bump the `points` stat once per unseen round. Stat-threshold
	// achievements (first_point, points_50, points_100) then fire automatically.
	public static void OnRoundResult( int pointsDelta, string roundId )
	{
		if ( pointsDelta <= 0 || string.IsNullOrEmpty( roundId ) ) return;
		var pd = PlayerData.Load();
		if ( pd.LastPointsRoundId == roundId ) return; // duplicate delivery / reconnect replay
		pd.LastPointsRoundId = roundId;
		pd.Save();
		try { Sandbox.Services.Stats.Increment( "points", pointsDelta ); }
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] points stat failed: {e.Message}" ); }
	}

	// game_over → placement achievements (idempotent Unlock) + the `wins` stat
	// once per unseen game (drives first_win / wins_5 / wins_10).
	public static void OnGameOver( int placement, bool won, string gameId )
	{
		try
		{
			if ( placement >= 1 && placement <= 5 ) Sandbox.Services.Achievements.Unlock( "top_5" );
			if ( placement >= 1 && placement <= 3 ) Sandbox.Services.Achievements.Unlock( "top_3" );
		}
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] unlock failed: {e.Message}" ); }

		if ( !won || string.IsNullOrEmpty( gameId ) ) return;
		var pd = PlayerData.Load();
		if ( pd.LastWinGameId == gameId ) return;
		pd.LastWinGameId = gameId;
		pd.Save();
		try { Sandbox.Services.Stats.Increment( "wins", 1 ); }
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] wins stat failed: {e.Message}" ); }
	}
}
