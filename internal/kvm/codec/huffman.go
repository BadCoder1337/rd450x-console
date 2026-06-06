package codec

// huffman.go — port of soc/video/HuffmanTable.java and the table-loading
// helpers in Decoder.java (loadHuffmanTable, loadHuffmanTableYDC/YAC,
// initHuffmanTable).

// huffmanTable mirrors com.ami.kvm.jviewer.soc.video.HuffmanTable.
type huffmanTable struct {
	Length    [17]int8     // code lengths per bit-length (Java byte[17])
	minorCode [17]int16    // Java short[17]
	majorCode [17]int16    // Java short[17]
	V         [65536]int16 // value lookup (Java short[65536])
	Len       [65536]byte  // bit-length lookup keyed by 16-bit code (Java byte[65536])
}

func newHuffmanTable() *huffmanTable { return &huffmanTable{} }

// wordHiLo ports Decoder.WORD_hi_lo: (getShort(hi)<<8) | getShort(lo).
// getShort(b) is (b & 0xFF).
func wordHiLo(hi, lo int8) int16 {
	return int16((int(uint8(hi)) << 8) | int(uint8(lo)))
}

// getInt ports Decoder.getInt: i & 0xFFFF.
func getInt16(v int16) int { return int(uint16(v)) }

// loadHuffmanTable ports Decoder.loadHuffmanTable(ht, nrcodes, values, huffcode).
func loadHuffmanTable(ht *huffmanTable, nrcodes [17]int8, values []int16, huffcode []int) *huffmanTable {
	for b := 1; b <= 16; b++ {
		ht.Length[b] = nrcodes[b]
	}
	i := 0
	for b4 := 1; b4 <= 16; b4++ {
		for b6 := 0; b6 < int(uint8(ht.Length[b4])); b6++ {
			ht.V[getInt16(wordHiLo(int8(b4), int8(b6)))] = values[i]
			i++
		}
	}
	i2 := 0
	for b8 := 1; b8 <= 16; b8++ {
		ht.minorCode[b8] = int16(i2)
		for b10 := 1; b10 <= int(uint8(ht.Length[b8])); b10++ {
			i2++
		}
		ht.majorCode[b8] = int16(i2 - 1)
		i2 *= 2
		if int(uint8(ht.Length[b8])) == 0 {
			ht.minorCode[b8] = -1
			ht.majorCode[b8] = 0
		}
	}
	// Build the Len[] lookup table from the (code,length) pairs.
	ht.Len[0] = 2
	i3 := 2
	for i4 := 1; i4 < 65535; i4++ {
		if i4 < huffcode[i3] {
			ht.Len[i4] = byte(huffcode[i3+1] & 255)
		} else {
			i3 += 2
			ht.Len[i4] = byte(huffcode[i3+1] & 255)
		}
	}
	return ht
}

// initHuffmanTables ports Decoder.initHuffmanTable.
func initHuffmanTables() (htDC, htAC [4]*huffmanTable) {
	for i := 0; i < 4; i++ {
		htDC[i] = newHuffmanTable()
		htAC[i] = newHuffmanTable()
	}
	htDC[0] = loadHuffmanTable(htDC[0], stdDCLuminanceNrcodes, stdDCLuminanceValues[:], dcLuminanceHuffmancode)
	htAC[0] = loadHuffmanTable(htAC[0], stdACLuminanceNrcodes, stdACLuminanceValues[:], acLuminanceHuffmancode)
	htDC[1] = loadHuffmanTable(htDC[1], stdDCChrominanceNrcodes, stdDCChrominanceValues[:], dcChrominanceHuffmancode)
	htAC[1] = loadHuffmanTable(htAC[1], stdACChrominanceNrcodes, stdACChrominanceValues[:], acChrominanceHuffmancode)
	return htDC, htAC
}
