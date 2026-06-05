package codec

// rc4.go — port of the RC4 layer in Decoder.java: Keys_Expansion,
// DecodeRC4_setup, RC4_crypt, plus the DecodeKeys constant.
//
// NOTE ON THE (byte) CASTS: the decompiled Java writes the running indices as
// `i2 = (byte)(i2 + 1)` etc. A real Java `byte` index that went negative would
// throw ArrayIndexOutOfBoundsException, so those casts stand in for the
// original C `& 0xFF` masking of the AST firmware's RC4. We port them as
// explicit 8-bit masks, which is the only interpretation that both matches
// standard RC4 and does not crash. (Decoder.java lines 1660-1695.)

// decodeKeys is Decoder.DecodeKeys = "fedcba9876543210".getBytes().
var decodeKeys = []byte("fedcba9876543210")

// rc4State mirrors Decoder.rc4_state { int x, y; int[256] m }.
type rc4State struct {
	x, y int
	m    [256]int
}

// keysExpansion ports Decoder.Keys_Expansion: it grows the key in place to 256
// bytes by repeating it (key[i] = key[i % len]). The Java method mutates the
// passed array, so the caller must run it on a 256-byte buffer seeded with the
// original key. We return a fresh 256-byte expanded key.
func keysExpansion(key []byte) []byte {
	n := len(key)
	out := make([]byte, 256)
	copy(out, key)
	for i := 0; i < 256; i++ {
		out[i] = out[i%n]
	}
	return out
}

// decodeRC4Setup ports Decoder.DecodeRC4_setup (the RC4 key schedule / KSA).
// expandedKey is the 256-byte array produced by keysExpansion.
func (s *rc4State) decodeRC4Setup(expandedKey []byte) {
	s.x = 0
	s.y = 0
	for i := 0; i < 256; i++ {
		s.m[i] = i
	}
	j := 0 // Java's `byte b`, masked to 8 bits
	keyIdx := 0
	for i := 0; i < 256; i++ {
		v := s.m[i]
		// b = (byte)(b + i4 + bArr[i2]); bArr is signed Java byte.
		j = (j + v + int(int8(expandedKey[keyIdx]))) & 0xFF
		s.m[i] = s.m[j]
		s.m[j] = v
		keyIdx++
	}
}

// rc4Crypt ports Decoder.RC4_crypt(int[] buf, int n): it XORs n words of buf
// (starting at VIRTADD, which is 0 here) with the RC4 keystream. Note the
// keystream is applied to whole 32-bit words, exactly as the Java does — the
// AST stream is keyed at word granularity, not byte granularity.
func (s *rc4State) rc4Crypt(buf []uint32, n int) {
	x := s.x
	y := s.y
	for i := 0; i < n; i++ {
		x = (x + 1) & 0xFF
		a := s.m[x]
		y = (y + a) & 0xFF
		b := s.m[y]
		s.m[x] = b
		s.m[y] = a
		buf[virtAdd+i] ^= uint32(s.m[(a+b)&0xFF])
	}
	s.x = x
	s.y = y
}
