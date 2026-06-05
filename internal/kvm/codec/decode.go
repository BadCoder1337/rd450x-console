package codec

// decode.go — port of Decoder.decode (the main macro-block dispatch loop) and
// its helpers decompressJPEG, decompressJPEGPass2, moveBlockIndex, plus the
// inline VQ-header reader used by the VQ block codes.

// decode ports Decoder.decode(VideoEngineInfo, int[]). h carries the parsed
// frame header; recv is the packed compressed stream (m_RecvBuffer).
func (d *Decoder) decode(h *frameHeader, recv []uint32) {
	// VQ defaults (Decoder.decode header).
	for i := 0; i < 4; i++ {
		d.vq.Index[i] = i
	}
	d.vq.Color[0] = 0x00008080
	d.vq.Color[1] = 0x00FF8080
	d.vq.Color[2] = 0x00808080
	d.vq.Color[3] = 0x00C08080

	d.width = h.destX
	d.height = h.destY
	d.realWidth = h.destX
	d.realH = h.destY
	d.mode420 = h.mode420

	if d.mode420 == 1 {
		d.width = roundUp(d.width, 16)
		d.height = roundUp(d.height, 16)
	} else {
		d.width = roundUp(d.width, 8)
		d.height = roundUp(d.height, 8)
	}

	d.tmpWidthBy16 = h.destX
	d.tmpHeightBy16 = h.destY
	if d.mode420 == 1 {
		d.tmpWidthBy16 = roundUp(d.tmpWidthBy16, 16)
		d.tmpHeightBy16 = roundUp(d.tmpHeightBy16, 16)
	} else {
		d.tmpWidthBy16 = roundUp(d.tmpWidthBy16, 8)
		d.tmpHeightBy16 = roundUp(d.tmpHeightBy16, 8)
	}

	d.ensureBuffers()

	compressWords := h.compressSize / 4

	// RC4 layer (Decoder.decode). The key schedule runs once per session;
	// RC4Enable per frame drives whether the crypt is applied.
	if h.rc4Enable == 1 {
		if !d.rc4SetupDone {
			expanded := keysExpansion(decodeKeys)
			d.rc4.decodeRC4Setup(expanded)
			d.rc4SetupDone = true
		}
		n := compressWords * 4
		if n > len(recv)-virtAdd {
			n = len(recv) - virtAdd
		}
		d.rc4.rc4Crypt(recv, n)
	}

	// Quant selectors.
	d.scaleFactor = 16
	d.scaleFactorUV = 16
	d.advanceScaleFactor = 16
	d.advanceScaleFactorUV = 16
	d.selector = h.jpegTableSelector
	d.advanceSelector = h.advanceTableSelector
	d.mapping = h.jpegYUVTableMapping

	d.loadLuminanceQuantizationTable(&d.qt[0])
	d.loadChrominanceQuantizationTable(&d.qt[1])
	d.loadPass2LuminanceQuantizationTable(&d.qt[2])
	d.loadPass2ChrominanceQuantizationTable(&d.qt[3])

	d.recv = recv
	d.index = 2
	d.tyb = 0
	d.txb = 0
	d.newbits = 32
	d.dcY = 0
	d.dcCb = 0
	d.dcCr = 0

	if len(recv) < 2 {
		return
	}

	for {
		code := (d.recv[0] >> 28) & 15
		switch code {
		case 0: // JPEG, no skip
			d.updateReadBuf(4)
			d.decompressJPEG(d.txb, d.tyb, 0)
			d.moveBlockIndex()
		case 8: // JPEG skip (reposition)
			d.readSkipPosition()
			d.updateReadBuf(20)
			d.decompressJPEG(d.txb, d.tyb, 0)
			d.moveBlockIndex()
		case 2: // JPEG pass2, no skip
			d.updateReadBuf(4)
			d.decompressJPEGPass2(d.txb, d.tyb, 2)
			d.moveBlockIndex()
		case 10: // JPEG pass2 skip
			d.readSkipPosition()
			d.updateReadBuf(20)
			d.decompressJPEGPass2(d.txb, d.tyb, 2)
			d.moveBlockIndex()
		case 5: // VQ 1-color, no skip
			d.updateReadBuf(4)
			d.readVQHeader(0, 1)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()
		case 13: // VQ 1-color, skip
			d.readSkipPosition()
			d.updateReadBuf(20)
			d.readVQHeader(0, 1)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()
		case 6: // VQ 2-color, no skip
			d.updateReadBuf(4)
			d.readVQHeader(1, 2)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()
		case 14: // VQ 2-color, skip
			d.readSkipPosition()
			d.updateReadBuf(20)
			d.readVQHeader(1, 2)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()
		case 7: // VQ 4-color, no skip
			d.updateReadBuf(4)
			d.readVQHeader(2, 4)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()
		case 15: // VQ 4-color, skip
			d.readSkipPosition()
			d.updateReadBuf(20)
			d.readVQHeader(2, 4)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()
		case 4: // low JPEG no skip (pass2 conversion, qt 2)
			d.updateReadBuf(4)
			d.decompressJPEG(d.txb, d.tyb, 2)
			d.moveBlockIndex()
		case 12: // low JPEG skip
			d.readSkipPosition()
			d.updateReadBuf(20)
			d.decompressJPEG(d.txb, d.tyb, 2)
			d.moveBlockIndex()
		case 9: // FRAME_END_CODE
			return
		default:
			// Unknown macro-block type: Java advances 3 bits and continues.
			d.updateReadBuf(3)
			d.moveBlockIndex()
		}
		if d.index >= compressWords {
			return
		}
	}
}

// readSkipPosition ports the txb/tyb extraction shared by all skip codes:
// txb = (recv[0] & 0x0FF00000) >> 20; tyb = (recv[0] & 0x000FF000) >> 12.
func (d *Decoder) readSkipPosition() {
	d.txb = int((d.recv[0] & 0x0FF00000) >> 20)
	d.tyb = int((d.recv[0] & 0x000FF000) >> 12)
}

// readVQHeader ports the inline VQ colour-cache update loop. bits sets
// m_VQ.BitMapBits; count is the number of cache slots to (re)load.
func (d *Decoder) readVQHeader(bits byte, count int) {
	d.vq.BitMapBits = bits
	for i := 0; i < count; i++ {
		d.vq.Index[i] = int((d.recv[0] >> 29) & 3)
		if ((d.recv[0] >> 31) & 1) == 0 {
			d.updateReadBuf(3) // VQ_NO_UPDATE_LENGTH
		} else {
			d.vq.Color[d.vq.Index[i]] = (d.recv[0] >> 5) & 0x00FFFFFF
			d.updateReadBuf(27) // VQ_UPDATE_LENGTH
		}
	}
}

// decompressJPEG ports Decoder.decompressJPEG(txb, tyb, b).
func (d *Decoder) decompressJPEG(tx, ty int, b byte) {
	d.dcY = d.decodeHuffmanDataUnit(d.yDCnr, d.yACnr, d.dcY, 0)
	d.inverseDCT(0, b)
	if d.mode420 == 1 {
		d.dcY = d.decodeHuffmanDataUnit(d.yDCnr, d.yACnr, d.dcY, 64)
		d.inverseDCT(64, b)
		d.dcY = d.decodeHuffmanDataUnit(d.yDCnr, d.yACnr, d.dcY, 128)
		d.inverseDCT(128, b)
		d.dcY = d.decodeHuffmanDataUnit(d.yDCnr, d.yACnr, d.dcY, 192)
		d.inverseDCT(192, b)
		d.dcCb = d.decodeHuffmanDataUnit(d.cbDCnr, d.cbACnr, d.dcCb, 256)
		d.inverseDCT(256, b+1)
		d.dcCr = d.decodeHuffmanDataUnit(d.crDCnr, d.crACnr, d.dcCr, 320)
		d.inverseDCT(320, b+1)
	} else {
		d.dcCb = d.decodeHuffmanDataUnit(d.cbDCnr, d.cbACnr, d.dcCb, 64)
		d.inverseDCT(64, b+1)
		d.dcCr = d.decodeHuffmanDataUnit(d.crDCnr, d.crACnr, d.dcCr, 128)
		d.inverseDCT(128, b+1)
	}
	d.convertYUVtoRGB(tx, ty)
}

// decompressJPEGPass2 ports Decoder.decompressJPEGPass2(txb, tyb, b).
func (d *Decoder) decompressJPEGPass2(tx, ty int, b byte) {
	d.dcY = d.decodeHuffmanDataUnit(d.yDCnr, d.yACnr, d.dcY, 0)
	d.inverseDCT(0, b)
	d.dcCb = d.decodeHuffmanDataUnit(d.cbDCnr, d.cbACnr, d.dcCb, 64)
	d.inverseDCT(64, b+1)
	d.dcCr = d.decodeHuffmanDataUnit(d.crDCnr, d.crACnr, d.dcCr, 128)
	d.inverseDCT(128, b+1)
	d.convertYUVToRGBPass2(tx, ty)
}

// moveBlockIndex ports Decoder.moveBlockIndex (advances txb/tyb across the tile
// grid; the GUI repaint calls are dropped).
func (d *Decoder) moveBlockIndex() {
	d.txb++
	if d.mode420 == 0 {
		if d.txb >= d.tmpWidthBy16/8 {
			d.tyb++
			if d.tyb >= d.tmpHeightBy16/8 {
				d.tyb = 0
			}
			d.txb = 0
		}
	} else {
		if d.txb >= d.tmpWidthBy16/16 {
			d.tyb++
			if d.tyb >= d.tmpHeightBy16/16 {
				d.tyb = 0
			}
			d.txb = 0
		}
	}
}

// ensureBuffers (re)allocates decodeBuf / prevYUV when the resolution changes,
// and sets the decodeBuf addressing stride for the current chroma mode.
func (d *Decoder) ensureBuffers() {
	if d.mode420 == 0 {
		d.bufStride = d.realWidth
	} else {
		d.bufStride = d.width
	}
	// Worst-case extent any tile may address: padded width/height.
	need := d.width * d.height * 3
	if need > d.bufAlloc {
		d.decodeBuf = make([]byte, need)
		d.prevYUV = make([]int32, need) // 3 ints/pixel, same count as decodeBuf bytes
		d.bufAlloc = need
	}
}

func roundUp(v, n int) int {
	if v%n != 0 {
		return v + n - v%n
	}
	return v
}
