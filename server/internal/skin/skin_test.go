package skin

import "testing"

// A minimal valid CEconItemPreviewDataBlock payload, hand-built:
//   00            leading 0x00 marker
//   18 07         field 3 (defindex) varint = 7
//   20 2c         field 4 (paintindex) varint = 44
//   00 00 00 00   trailing 4-byte CRC (ignored)
const goodHex = "001807202c00000000"

func TestDecodeBareHex(t *testing.T) {
	def, paint, ok := Decode(goodHex)
	if !ok || def != 7 || paint != 44 {
		t.Fatalf("Decode(bare) = (%d, %d, %v), want (7, 44, true)", def, paint, ok)
	}
}

func TestDecodeSteamLink(t *testing.T) {
	// The full link form: a steam:// preview action with the %20-separated hex.
	link := "steam://rungame/730/x/+csgo_econ_action_preview%20" + goodHex
	def, paint, ok := Decode(link)
	if !ok || def != 7 || paint != 44 {
		t.Fatalf("Decode(link) = (%d, %d, %v), want (7, 44, true)", def, paint, ok)
	}
}

func TestDecodeMasked(t *testing.T) {
	// XOR-masked links: every byte XOR'd with the key (here 0xAA), so the leading
	// 0x00 surfaces as the key on the wire and Decode must unmask it first.
	raw, _ := hexToBytes(goodHex)
	const key = 0xAA
	masked := make([]byte, len(raw))
	for i, b := range raw {
		masked[i] = b ^ key
	}
	if masked[0] == 0 {
		t.Fatal("masked payload should not start with 0x00")
	}
	def, paint, ok := Decode(bytesToHex(masked))
	if !ok || def != 7 || paint != 44 {
		t.Fatalf("Decode(masked) = (%d, %d, %v), want (7, 44, true)", def, paint, ok)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"",                   // empty
		"   ",                // blank
		"nothex!!",           // non-hex chars
		"abc",                // odd length
		"0011",               // too short
		"110018072c00000000", // valid hex but unmasks to junk (no defindex/paintindex)
	} {
		if _, _, ok := Decode(bad); ok {
			t.Errorf("Decode(%q) = ok, want not ok", bad)
		}
	}
}

func TestExtFor(t *testing.T) {
	cases := map[string]string{
		"https://x/y.png":         ".png",
		"https://x/y.JPG":         ".jpg",
		"https://x/y.webp?w=512":  ".webp",
		"https://x/y":             ".png", // no extension → default
		"https://x/y.bmp":         ".png", // disallowed → default
	}
	for url, want := range cases {
		if got := extFor(url); got != want {
			t.Errorf("extFor(%q) = %q, want %q", url, got, want)
		}
	}
}

// bytesToHex is a test helper (the package only needs hex→bytes at runtime).
func bytesToHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = digits[x>>4]
		out[i*2+1] = digits[x&0x0f]
	}
	return string(out)
}
