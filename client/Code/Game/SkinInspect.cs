using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Threading.Tasks;
using Sandbox;

namespace Splitclicker.Game;

// Decodes a CS2 inspect link locally and resolves the skin's display data for the
// HUD's "skin to win" panel.
//
// Since Valve's March 2026 change every inspect link self-encodes the item, so the
// wear float, paint seed and the numeric weapon/paint ids come straight out of the
// link with no Steam call. The link is (optionally XOR-masked with its own first
// byte) a leading 0x00 + a protobuf CEconItemPreviewDataBlock + a 4-byte CRC; we
// only read the four varint fields we care about (defindex=3, paintindex=4,
// paintwear=7, paintseed=8) and skip everything else by wire type.
//
// The human name + weapon image are NOT in the link, so they're looked up by
// (defindex, paintindex) in the community ByMykel/CSGO-API dataset, which is
// fetched once and cached to local storage (re-pulled when older than a week).
// Any failure leaves the caller to fall back to the server-served skin image.
public static class SkinInspect
{
	// ── decoded straight from the link (no network) ──
	public struct Item
	{
		public bool Ok;        // false → the link couldn't be parsed
		public int DefIndex;
		public int PaintIndex;
		public int PaintSeed;
		public float Float;
	}

	// ── fully resolved skin handed to the HUD ──
	public class Skin
	{
		public bool DecodeOk;   // link decoded → Float/PaintSeed/WearName are valid
		public bool ImageOk;    // dataset resolved → Name/ImageUrl are valid
		public string Name = "";
		public string ImageUrl = "";
		public string WearName = "";
		public float Float;
		public int PaintSeed;
	}

	const string DatasetUrl = "https://raw.githubusercontent.com/ByMykel/CSGO-API/main/public/api/en/skins.json";
	const string CacheFile = "skins_dataset.json";
	const string CacheStampFile = "skins_dataset.stamp"; // DateTime.UtcNow.Ticks of last fetch
	static readonly TimeSpan MaxAge = TimeSpan.FromDays( 7 );

	static readonly JsonSerializerOptions JsonOpts = new() { PropertyNameCaseInsensitive = true };

	// Lazily-built (defindex,paintindex) → {name,image} index, shared across calls.
	static Dictionary<long, Entry> _index;
	static Task<Dictionary<long, Entry>> _indexTask;

	struct Entry { public string Name; public string Image; }

	// Resolve a link to everything the HUD needs. Never throws: on any failure the
	// returned Skin has DecodeOk/ImageOk false so the caller shows the fallback.
	public static async Task<Skin> Resolve( string inspectLink )
	{
		var skin = new Skin();
		var item = Decode( inspectLink );
		if ( !item.Ok ) return skin;

		skin.DecodeOk = true;
		skin.Float = item.Float;
		skin.PaintSeed = item.PaintSeed;
		skin.WearName = WearName( item.Float );

		try
		{
			var index = await LoadIndex();
			if ( index != null && index.TryGetValue( Key( item.DefIndex, item.PaintIndex ), out var e ) )
			{
				skin.ImageOk = true;
				skin.Name = e.Name;
				skin.ImageUrl = e.Image;
			}
		}
		catch ( Exception ex )
		{
			Log.Warning( $"[Splitclicker] skin dataset lookup failed: {ex.Message}" );
		}
		return skin;
	}

	// ── link decoding ────────────────────────────────────────────────────────

	// Decode an inspect link (a full steam://… link or a bare hex payload) into its
	// item fields. Returns Ok=false on anything malformed.
	public static Item Decode( string link )
	{
		var item = new Item();
		var hex = ExtractHex( link );
		if ( hex == null ) return item;

		var buf = HexToBytes( hex );
		if ( buf == null || buf.Length < 6 ) return item;

		// Masked links are XOR'd with their own first byte (so the real leading
		// 0x00 becomes key^key=0 on the wire); unmask in place if so.
		if ( buf[0] != 0 )
		{
			byte key = buf[0];
			for ( int i = 0; i < buf.Length; i++ ) buf[i] ^= key;
		}
		if ( buf[0] != 0 ) return item; // must start with the 0x00 marker once unmasked

		// Payload sits between the leading 0x00 and the trailing 4-byte CRC.
		ParseProto( buf, 1, buf.Length - 4, ref item );
		item.Ok = item.DefIndex != 0 && item.PaintIndex != 0;
		return item;
	}

	// Pull the trailing hex token out of whatever the admin pasted: a full
	// steam://…+csgo_econ_action_preview%20<HEX> link, the space-separated form, or
	// just the bare hex. Returns null when no usable hex is found.
	static string ExtractHex( string s )
	{
		if ( string.IsNullOrWhiteSpace( s ) ) return null;
		s = s.Trim().Replace( "%20", " " );
		int sp = s.LastIndexOf( ' ' );
		if ( sp >= 0 ) s = s.Substring( sp + 1 );
		s = s.Trim();
		if ( s.Length < 12 || (s.Length & 1) != 0 ) return null;
		foreach ( var c in s )
			if ( HexVal( c ) < 0 ) return null;
		return s;
	}

	static byte[] HexToBytes( string s )
	{
		var b = new byte[s.Length / 2];
		for ( int i = 0; i < b.Length; i++ )
		{
			int hi = HexVal( s[i * 2] ), lo = HexVal( s[i * 2 + 1] );
			if ( hi < 0 || lo < 0 ) return null;
			b[i] = (byte)((hi << 4) | lo);
		}
		return b;
	}

	static int HexVal( char c )
	{
		if ( c >= '0' && c <= '9' ) return c - '0';
		if ( c >= 'a' && c <= 'f' ) return c - 'a' + 10;
		if ( c >= 'A' && c <= 'F' ) return c - 'A' + 10;
		return -1;
	}

	// Minimal protobuf reader: read the varint fields we care about, skip the rest
	// by wire type. defindex=3, paintindex=4, paintwear=7 (float bits), paintseed=8.
	static void ParseProto( byte[] b, int i, int end, ref Item it )
	{
		while ( i < end )
		{
			ulong tag = ReadVarint( b, ref i, end, out bool ok );
			if ( !ok ) return;
			int field = (int)(tag >> 3);
			int wire = (int)(tag & 7);
			switch ( wire )
			{
				case 0: // varint
					ulong v = ReadVarint( b, ref i, end, out ok );
					if ( !ok ) return;
					switch ( field )
					{
						case 3: it.DefIndex = (int)v; break;
						case 4: it.PaintIndex = (int)v; break;
						case 7: it.Float = UInt32BitsToFloat( (uint)v ); break;
						case 8: it.PaintSeed = (int)v; break;
					}
					break;
				case 1: i += 8; break; // 64-bit
				case 5: i += 4; break; // 32-bit
				case 2: // length-delimited → skip
					ulong len = ReadVarint( b, ref i, end, out ok );
					if ( !ok ) return;
					i += (int)len;
					break;
				default: return; // groups / unknown wire type → bail
			}
		}
	}

	static ulong ReadVarint( byte[] b, ref int i, int end, out bool ok )
	{
		ulong result = 0;
		int shift = 0;
		while ( i < end && shift < 64 )
		{
			byte c = b[i++];
			result |= (ulong)(c & 0x7f) << shift;
			if ( (c & 0x80) == 0 ) { ok = true; return result; }
			shift += 7;
		}
		ok = false;
		return 0;
	}

	// Reinterpret a uint32's bits as an IEEE-754 single (paintwear's encoding).
	// GetBytes/ToSingle round-trip uses the same endianness, so it's platform-safe.
	static float UInt32BitsToFloat( uint bits ) => BitConverter.ToSingle( BitConverter.GetBytes( bits ), 0 );

	// Standard CS2 wear buckets by float.
	static string WearName( float f ) =>
		f < 0.07f ? "Factory New" :
		f < 0.15f ? "Minimal Wear" :
		f < 0.38f ? "Field-Tested" :
		f < 0.45f ? "Well-Worn" : "Battle-Scarred";

	// ── dataset index ────────────────────────────────────────────────────────

	static long Key( int defIndex, int paintIndex ) => ((long)defIndex << 20) | (uint)paintIndex;

	static async Task<Dictionary<long, Entry>> LoadIndex()
	{
		if ( _index != null ) return _index;
		// Share one in-flight build across concurrent callers, but clear it on
		// failure so a later resolve can retry rather than reuse a faulted task.
		_indexTask ??= BuildIndex();
		try { return await _indexTask; }
		catch { _indexTask = null; throw; }
	}

	static async Task<Dictionary<long, Entry>> BuildIndex()
	{
		var json = await LoadDatasetText();
		var list = JsonSerializer.Deserialize<List<RawSkin>>( json, JsonOpts );
		var index = new Dictionary<long, Entry>();
		if ( list != null )
		{
			foreach ( var s in list )
			{
				if ( s?.Weapon == null || string.IsNullOrEmpty( s.PaintIndex ) ) continue;
				if ( !int.TryParse( s.PaintIndex, out int pi ) ) continue;
				index[Key( s.Weapon.WeaponId, pi )] = new Entry { Name = s.Name, Image = s.Image };
			}
		}
		_index = index;
		return index;
	}

	// Return the dataset JSON text: the local cache when present and fresh, else a
	// fresh download (re-cached), else a stale cache as a last resort.
	static async Task<string> LoadDatasetText()
	{
		if ( CacheFresh() )
		{
			try { return FileSystem.Data.ReadAllText( CacheFile ); }
			catch { /* fall through to refetch */ }
		}

		string fetched = null;
		try
		{
			var resp = await Http.RequestAsync( DatasetUrl, "GET", null );
			if ( resp.IsSuccessStatusCode )
				fetched = await resp.Content.ReadAsStringAsync();
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] skin dataset fetch failed: {e.Message}" );
		}

		if ( !string.IsNullOrEmpty( fetched ) )
		{
			try
			{
				FileSystem.Data.WriteAllText( CacheFile, fetched );
				FileSystem.Data.WriteAllText( CacheStampFile, DateTime.UtcNow.Ticks.ToString() );
			}
			catch { /* caching is best-effort */ }
			return fetched;
		}

		// Fetch failed — use any cached copy, however old, rather than give up.
		if ( FileSystem.Data.FileExists( CacheFile ) )
			return FileSystem.Data.ReadAllText( CacheFile );

		throw new Exception( "skin dataset unavailable" );
	}

	static bool CacheFresh()
	{
		try
		{
			if ( !FileSystem.Data.FileExists( CacheFile ) || !FileSystem.Data.FileExists( CacheStampFile ) )
				return false;
			if ( !long.TryParse( FileSystem.Data.ReadAllText( CacheStampFile ), out long ticks ) )
				return false;
			var age = DateTime.UtcNow - new DateTime( ticks, DateTimeKind.Utc );
			return age >= TimeSpan.Zero && age < MaxAge;
		}
		catch { return false; }
	}

	// Just the dataset fields we use; everything else in the entry is ignored.
	class RawSkin
	{
		[JsonPropertyName( "name" )] public string Name { get; set; }
		[JsonPropertyName( "image" )] public string Image { get; set; }
		[JsonPropertyName( "paint_index" )] public string PaintIndex { get; set; }
		[JsonPropertyName( "weapon" )] public RawWeapon Weapon { get; set; }
	}

	class RawWeapon
	{
		[JsonPropertyName( "weapon_id" )] public int WeaponId { get; set; }
	}
}
