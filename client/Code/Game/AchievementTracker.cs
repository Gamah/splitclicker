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

		// "Ahead of the Curve": more than 5 scoring clicks in a single round. Manual
		// Unlock is idempotent, so it's safe to attempt on every delivery (no round_id
		// guard needed) — unlike the `points` increment below.
		if ( pointsDelta > 5 ) Unlock( "ahead_of_the_curve" );

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
		if ( placement >= 1 && placement <= 5 ) Unlock( "top_5" );
		if ( placement >= 1 && placement <= 3 ) Unlock( "top_3" );
		// "Chicken Dinner": win a session (finish #1 in the game's final standings).
		if ( won ) Unlock( "chicken_dinner" );

		if ( !won || string.IsNullOrEmpty( gameId ) ) return;
		var pd = PlayerData.Load();
		if ( pd.LastWinGameId == gameId ) return;
		pd.LastWinGameId = gameId;
		pd.Save();
		try { Sandbox.Services.Stats.Increment( "wins", 1 ); }
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] wins stat failed: {e.Message}" ); }
	}

	// Idempotent achievement unlock with a console trace. Manual unlocks give no
	// visible feedback in the editor (the ident must be defined on the published
	// package to actually pop), so the log line is how we confirm it fired.
	static void Unlock( string ident )
	{
		Log.Info( $"[Splitclicker] fired achievement {ident}" );
		try { Sandbox.Services.Achievements.Unlock( ident ); }
		catch ( Exception e ) { Log.Warning( $"[Splitclicker] unlock {ident} failed: {e.Message}" ); }
	}
}
