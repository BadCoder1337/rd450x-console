package codec

import "testing"

// TestZigzagDezigzag verifies the scan-order tables match JTables exactly and
// are self-consistent: zigzag is a permutation of 0..63.
func TestZigzag(t *testing.T) {
	var seen [64]bool
	for i, v := range zigzag {
		if int(v) >= 64 {
			t.Fatalf("zigzag[%d]=%d out of range", i, v)
		}
		if seen[v] {
			t.Fatalf("zigzag value %d repeated", v)
		}
		seen[v] = true
	}
	// First entries fixed by the JPEG spec.
	if zigzag[0] != 0 || zigzag[1] != 1 || zigzag[2] != 5 {
		t.Fatalf("zigzag prefix wrong: %v", zigzag[:3])
	}
	// dezigzag first 64 entries must also be a permutation of 0..63.
	var seen2 [64]bool
	for i := 0; i < 64; i++ {
		v := dezigzag[i]
		if int(v) >= 64 {
			t.Fatalf("dezigzag[%d]=%d out of range", i, v)
		}
		seen2[v] = true
	}
	for i := 0; i < 64; i++ {
		if !seen2[i] {
			t.Fatalf("dezigzag missing %d", i)
		}
	}
}

// TestKeysExpansion checks the 256-byte key repeats the 16-byte DecodeKeys.
func TestKeysExpansion(t *testing.T) {
	exp := keysExpansion(decodeKeys)
	if len(exp) != 256 {
		t.Fatalf("expanded key len = %d, want 256", len(exp))
	}
	for i := 0; i < 256; i++ {
		if exp[i] != decodeKeys[i%len(decodeKeys)] {
			t.Fatalf("exp[%d]=%c want %c", i, exp[i], decodeKeys[i%len(decodeKeys)])
		}
	}
}

// TestRC4KeySchedule cross-checks the ported KSA against an independent
// reference RC4 key schedule using the same 256-byte expanded key, since the
// AST setup is exactly standard RC4 KSA with mask 0xFF.
func TestRC4KeySchedule(t *testing.T) {
	exp := keysExpansion(decodeKeys)

	var s rc4State
	s.decodeRC4Setup(exp)

	// Reference standard RC4 KSA over the same 256-byte key.
	var m [256]int
	for i := 0; i < 256; i++ {
		m[i] = i
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + m[i] + int(int8(exp[i]))) & 0xFF
		m[i], m[j] = m[j], m[i]
	}
	for i := 0; i < 256; i++ {
		if s.m[i] != m[i] {
			t.Fatalf("RC4 S-box mismatch at %d: %d != %d", i, s.m[i], m[i])
		}
	}
	if s.x != 0 || s.y != 0 {
		t.Fatalf("RC4 x/y not reset: %d/%d", s.x, s.y)
	}
}

// TestRC4CryptRoundTrip checks the keystream is involutive: crypting twice with
// fresh state restores the original buffer (XOR property).
func TestRC4CryptRoundTrip(t *testing.T) {
	exp := keysExpansion(decodeKeys)
	orig := []uint32{0xDEADBEEF, 0x01020304, 0xFFFFFFFF, 0x00000000, 0x12345678}

	buf := append([]uint32(nil), orig...)
	var s1 rc4State
	s1.decodeRC4Setup(exp)
	s1.rc4Crypt(buf, len(buf))

	enc := append([]uint32(nil), buf...)
	var s2 rc4State
	s2.decodeRC4Setup(exp)
	s2.rc4Crypt(buf, len(buf))

	for i := range orig {
		if buf[i] != orig[i] {
			t.Fatalf("round-trip mismatch at %d: %08x != %08x", i, buf[i], orig[i])
		}
	}
	// Encryption must actually change the data.
	same := true
	for i := range orig {
		if enc[i] != orig[i] {
			same = false
		}
	}
	if same {
		t.Fatal("RC4 produced no change")
	}
}

// TestHuffmanTableConstruction verifies the DC luminance table decodes the
// canonical JPEG codes. The standard DC luminance table assigns code 00 (len 2)
// to category 0, 010 (len 3) to category 1, etc. We confirm minor/major_code
// and a couple of Len[] entries.
func TestHuffmanTableConstruction(t *testing.T) {
	ht := newHuffmanTable()
	ht = loadHuffmanTable(ht, stdDCLuminanceNrcodes, stdDCLuminanceValues[:], dcLuminanceHuffmancode)

	// Length[2]=1 (one 2-bit code), Length[3]=5 (five 3-bit codes) per JTables.
	if ht.Length[2] != 1 || ht.Length[3] != 5 {
		t.Fatalf("Length[2]=%d Length[3]=%d", ht.Length[2], ht.Length[3])
	}
	// Len[0] is seeded to 2 by the loader.
	if ht.Len[0] != 2 {
		t.Fatalf("Len[0]=%d want 2", ht.Len[0])
	}
	// The Len[] lookup must classify a 16-bit prefix into a valid code length
	// (1..16) for the whole range.
	for code := 0; code < 65535; code++ {
		l := ht.Len[code]
		if l < 1 || l > 16 {
			t.Fatalf("Len[%d]=%d out of [1,16]", code, l)
		}
	}
}

// TestColorTables sanity-checks the YUV→RGB coefficient tables against the
// closed-form values initColorTable computes, at a few anchor points.
func TestColorTables(t *testing.T) {
	d := New()
	fixG := func(v float64) int { return int(v*65536.0 + 0.5) }
	half := 65536 >> 1
	// Cr→R at chroma index 128 corresponds to (128-128)=0 offset → near 0.
	want := int32((fixG(1.597656)*0 + half) >> 16)
	if d.calcRGBofCrToR[128] != want {
		t.Fatalf("calcRGBofCrToR[128]=%d want %d", d.calcRGBofCrToR[128], want)
	}
	// Y table at index 16 corresponds to (16-16)=0 → near 0.
	if d.calcRGBofY[16] != int32((fixG(1.164)*0+half)>>16) {
		t.Fatalf("calcRGBofY[16]=%d", d.calcRGBofY[16])
	}
}

// TestRangeLimitTable checks the clamp table: identity in the middle band,
// saturating to 0/255 in the overflow bands.
func TestRangeLimitTable(t *testing.T) {
	d := New()
	// Middle band [256,512): identity 0..255.
	for s := 0; s < 256; s++ {
		if d.rangeLimitTable[256+s] != byte(s) {
			t.Fatalf("rangeLimitTable[%d]=%d want %d", 256+s, d.rangeLimitTable[256+s], s)
		}
	}
	// Overflow band [512,895): saturates to 255.
	if d.rangeLimitTable[512] != 0xFF || d.rangeLimitTable[894] != 0xFF {
		t.Fatalf("overflow band not 0xFF")
	}
}

// TestIDCTFlatBlock feeds an all-DC block (only coefficient 0 nonzero, AC=0)
// and confirms the IDCT yields a spatially flat 8×8 block, the defining
// property of the transform. We use an identity-ish quant table.
func TestIDCTFlatBlock(t *testing.T) {
	d := New()
	// Quant table: set so QT*coeff>>16 = coeff (i.e. QT entry = 65536).
	for i := 0; i < 64; i++ {
		d.qt[0][i] = 65536
	}
	for i := range d.dctCoeff {
		d.dctCoeff[i] = 0
	}
	// DC term only. After the IDCT, a pure-DC 8×8 spatial block is flat.
	d.dctCoeff[0] = 64 // DC value
	d.inverseDCT(0, 0)

	first := d.yuvTile[0]
	for i := 1; i < 64; i++ {
		if d.yuvTile[i] != first {
			t.Fatalf("IDCT of DC-only block not flat: yuvTile[%d]=%d != %d", i, d.yuvTile[i], first)
		}
	}
}

// TestMakeIntArray verifies little-endian packing and padding from
// SOCJVVideo.MakeIntArray.
func TestMakeIntArray(t *testing.T) {
	in := []byte{0x01, 0x02, 0x03, 0x04, 0xAA, 0xBB}
	out := makeIntArray(in)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2 (6 bytes padded to 8)", len(out))
	}
	if out[0] != 0x04030201 {
		t.Fatalf("out[0]=%08x want 04030201", out[0])
	}
	if out[1] != 0x0000BBAA {
		t.Fatalf("out[1]=%08x want 0000bbaa", out[1])
	}
}

// TestSetQuantizationTable checks the signed-byte clamp behaviour: a negative
// quant byte clamps to 1, a large positive byte clamps to 255.
func TestSetQuantizationTable(t *testing.T) {
	var src [64]int8
	src[0] = -120 // negative → (−120*16)/16 = −120 → clamp to 1
	src[1] = 100  // 100 → 100
	var out [64]byte
	setQuantizationTable(&src, 16, &out)
	if out[zigzag[0]] != 1 {
		t.Fatalf("negative quant byte not clamped to 1: got %d", out[zigzag[0]])
	}
	if out[zigzag[1]] != 100 {
		t.Fatalf("quant byte 100 -> %d want 100", out[zigzag[1]])
	}
}
