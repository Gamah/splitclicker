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
	public const string ProdUrl = "https://splitclicker.example.com";

	/// <summary>Backend root. Mutable so a dev build can point at localhost.</summary>
	public static string BaseUrl { get; set; } = ProdUrl;

	/// <summary>WebSocket URL for the given ticket, derived from BaseUrl
	/// (https→wss, http→ws). The ticket is the only thing on the URL.</summary>
	public static string WsUrl( string ticket )
	{
		var scheme = BaseUrl.StartsWith( "https" ) ? "wss" : "ws";
		var host = BaseUrl.Substring( BaseUrl.IndexOf( "://" ) + 3 );
		return $"{scheme}://{host}/ws?ticket={Uri.EscapeDataString( ticket )}";
	}

	static readonly JsonSerializerOptions JsonOpts = new() { PropertyNameCaseInsensitive = true };

	/// <summary>Local SteamID64 as a string, or null on non-Steam/web builds.
	/// Sent as a string — it exceeds JS/double precision.</summary>
	static string LocalSteamId()
	{
		ulong id = Connection.Local?.SteamId ?? 0UL;
		return id != 0 ? id.ToString() : null;
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

		try
		{
			var resp = await Http.RequestAsync( BaseUrl + "/api/v1/auth", "POST", Http.CreateJsonContent( body ) );
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

	/// <summary>Current UTC-hour leaderboard (top `limit`). Empty list on failure.</summary>
	public static async Task<List<Standing>> GetHourlyLeaderboard( int limit = 100 )
	{
		try
		{
			var resp = await Http.RequestAsync( BaseUrl + $"/api/v1/leaderboard/hourly?limit={limit}", "GET", null );
			if ( !resp.IsSuccessStatusCode ) return new List<Standing>();
			return JsonSerializer.Deserialize<List<Standing>>( await resp.Content.ReadAsStringAsync(), JsonOpts )
				?? new List<Standing>();
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] leaderboard fetch failed: {e.Message}" );
			return new List<Standing>();
		}
	}
}
