using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Threading.Tasks;
using Sandbox;

namespace Splitclicker.Api;

// Thin HTTP client. Identity is the Steam account: the client mints a Facepunch
// auth token and the backend validates it (no GUID enrollment, no OpenID). One
// call — Auth — proves identity and returns a single-use WebSocket ticket.
public static class ApiClient
{
	public const string ProdUrl = "https://fart.notadomain.lol";

	/// <summary>Backend root. Mutable so a dev build can point at localhost.</summary>
	public static string BaseUrl { get; set; } = ProdUrl;

	/// <summary>API version segment used in every REST path and the WS path
	/// (/api/{ver}/… and /ws/{ver}). "v4" is the current build (anticheat test gate +
	/// the cooldown/ignored sanction ladder); set it to "v3"/"v2"/"v1" to exercise an
	/// older or the legacy/troll path the server gives clients below its configured
	/// live version. An EMPTY string means no segment at all — raw /ws — for the
	/// legacy/unversioned old master. Mutable so the scene's ClickController can
	/// override it for testing.</summary>
	public static string ApiVersion { get; set; } = "v4";

	/// <summary>The version path segment, e.g. "/v2", or "" when <see cref="ApiVersion"/>
	/// is blank (raw, unversioned paths). Inserted into both REST and WS URLs.</summary>
	static string VerSeg => string.IsNullOrWhiteSpace( ApiVersion ) ? "" : "/" + ApiVersion.Trim().Trim( '/' );

	/// <summary>WebSocket URL for the given ticket, derived from BaseUrl
	/// (https→wss, http→ws). The ticket is the only thing on the URL.</summary>
	public static string WsUrl( string ticket )
	{
		var scheme = BaseUrl.StartsWith( "https" ) ? "wss" : "ws";
		var host = BaseUrl.Substring( BaseUrl.IndexOf( "://" ) + 3 );
		return $"{scheme}://{host}/ws{VerSeg}?ticket={Uri.EscapeDataString( ticket )}";
	}

	static readonly JsonSerializerOptions JsonOpts = new() { PropertyNameCaseInsensitive = true };

	/// <summary>Local SteamID64 as a string, or null on non-Steam/web builds.
	/// Sent as a string — it exceeds JS/double precision.</summary>
	static string LocalSteamId()
	{
		ulong id = Connection.Local?.SteamId ?? 0UL;
		return id != 0 ? id.ToString() : null;
	}

	/// <summary>Local Steam display name, or null if unavailable. Reported at auth
	/// so the board can show a real name instead of the opaque hex tag.</summary>
	static string LocalSteamName()
	{
		var name = Connection.Local?.DisplayName;
		return string.IsNullOrWhiteSpace( name ) ? null : name;
	}

	/// <summary>Facepunch token proving Steam ownership. Returns null (never throws)
	/// when none can be minted (web/non-Steam) so the caller fails cleanly.</summary>
	static async Task<string> AuthToken()
	{
		try { return await Sandbox.Services.Auth.GetToken( "splitclicker" ); }
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] auth token unavailable: {e.Message}" );
			return null;
		}
	}

	/// <summary>Prove the Steam identity and get a WS ticket. username is optional
	/// (sets/updates the display name). Returns null on any failure — the caller
	/// should surface "couldn't connect" rather than proceed unauthenticated.</summary>
	public static async Task<AuthResponse> Auth( string username = null )
	{
		var steamId = LocalSteamId();
		if ( steamId == null )
		{
			Log.Warning( "[Splitclicker] no SteamID — Steam is required to play" );
			return null;
		}
		var token = await AuthToken();
		if ( string.IsNullOrEmpty( token ) ) return null;

		var body = new Dictionary<string, string> { ["steam_id"] = steamId, ["token"] = token };
		if ( !string.IsNullOrEmpty( username ) ) body["username"] = username;
		var steamName = LocalSteamName();
		if ( steamName != null ) body["display_name"] = steamName;

		try
		{
			var resp = await Http.RequestAsync( BaseUrl + $"/api{VerSeg}/auth", "POST", Http.CreateJsonContent( body ) );
			if ( !resp.IsSuccessStatusCode )
			{
				Log.Warning( $"[Splitclicker] auth failed: HTTP {(int)resp.StatusCode}" );
				return null;
			}
			return JsonSerializer.Deserialize<AuthResponse>( await resp.Content.ReadAsStringAsync(), JsonOpts );
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] auth request error: {e.Message}" );
			return null;
		}
	}

	/// <summary>Absolute URL for a server-relative path (e.g. "/api/v1/skin")
	/// returned by config. Pass-through if already absolute; null in/out.</summary>
	public static string AbsoluteUrl( string path )
	{
		if ( string.IsNullOrEmpty( path ) ) return null;
		if ( path.StartsWith( "http" ) ) return path;
		return BaseUrl.TrimEnd( '/' ) + "/" + path.TrimStart( '/' );
	}

	/// <summary>Server-driven startup config (winner-lock time + skin image URL).
	/// Returns null on failure; the caller falls back to sensible defaults.</summary>
	public static async Task<ConfigResponse> GetConfig()
	{
		try
		{
			var resp = await Http.RequestAsync( BaseUrl + $"/api{VerSeg}/config", "GET", null );
			if ( !resp.IsSuccessStatusCode ) return null;
			return JsonSerializer.Deserialize<ConfigResponse>( await resp.Content.ReadAsStringAsync(), JsonOpts );
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] config fetch failed: {e.Message}" );
			return null;
		}
	}

	/// <summary>The recently settled bounties (newest-won first, up to 5) for the
	/// "previous winner" panel — each with its winner and skin. Empty list on
	/// failure; the panel just hides.</summary>
	public static async Task<List<PreviousBounty>> GetPreviousBounties()
	{
		try
		{
			var resp = await Http.RequestAsync( BaseUrl + $"/api{VerSeg}/bounties/previous", "GET", null );
			if ( !resp.IsSuccessStatusCode ) return new List<PreviousBounty>();
			return JsonSerializer.Deserialize<List<PreviousBounty>>( await resp.Content.ReadAsStringAsync(), JsonOpts )
				?? new List<PreviousBounty>();
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] previous bounties fetch failed: {e.Message}" );
			return new List<PreviousBounty>();
		}
	}

	/// <summary>Current UTC-hour leaderboard (top `limit`). Empty list on failure.</summary>
	public static Task<List<Standing>> GetHourlyLeaderboard( int limit = 100 ) =>
		GetLeaderboard( "hourly", limit );

	/// <summary>Career "hours won" leaderboard (top `limit`). Empty list on failure.
	/// Each Standing's Points is the hours-won count.</summary>
	public static Task<List<Standing>> GetHoursWonLeaderboard( int limit = 100 ) =>
		GetLeaderboard( "hours-won", limit );

	/// <summary>"Games won this bounty" leaderboard (top `limit`) — games won in the
	/// active bounty's window. Empty list on failure. Each Standing's Points is the
	/// games-won count.</summary>
	public static Task<List<Standing>> GetSessionsWonLeaderboard( int limit = 100 ) =>
		GetLeaderboard( "sessions-won", limit );

	/// <summary>All-time "top clickers" leaderboard (top `limit`) — total scoring
	/// clicks across all bounties; never resets. Empty list on failure. Each
	/// Standing's Points is the lifetime click count.</summary>
	public static Task<List<Standing>> GetAllTimeClickersLeaderboard( int limit = 100 ) =>
		GetLeaderboard( "all-time-clicks", limit );

	static async Task<List<Standing>> GetLeaderboard( string board, int limit )
	{
		try
		{
			var resp = await Http.RequestAsync( BaseUrl + $"/api{VerSeg}/leaderboard/{board}?limit={limit}", "GET", null );
			if ( !resp.IsSuccessStatusCode ) return new List<Standing>();
			return JsonSerializer.Deserialize<List<Standing>>( await resp.Content.ReadAsStringAsync(), JsonOpts )
				?? new List<Standing>();
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] {board} leaderboard fetch failed: {e.Message}" );
			return new List<Standing>();
		}
	}
}
