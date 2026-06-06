package codec

// yuv.go — port of Decoder.convertYUVtoRGB and convertYUVToRGBPass2. Output is
// written to d.decodeBuf as 24-bit BGR (3 bytes/pixel: [B,G,R]).
//
// The GUI side effects in the Java (m_socvidClnt.SetPointInSavedScreen, used
// only for the hardware-cursor overlay, and m_view.repaint) are intentionally
// dropped — they do not affect the decoded pixels.

// convertYUVtoRGB ports Decoder.convertYUVtoRGB(txb, tyb).
func (d *Decoder) convertYUVtoRGB(tx, ty int) {
	if d.mode420 == 0 {
		// 4:4:4 path (8×8 tile).
		copy(d.yTile[:], d.yuvTile[0:64])
		for i := 0; i < 64; i++ {
			d.cbTile[i] = d.yuvTile[64+i]
			d.crTile[i] = d.yuvTile[128+i]
		}
		x0 := tx * 8
		y0 := ty * 8
		off := y0*d.realWidth + x0
		limit := d.realWidth - x0
		if limit == 0 || limit > 8 {
			limit = 8
		}
		for r := 0; r < 8; r++ {
			for c := 0; c < limit; c++ {
				idx := (r << 3) + c
				pi := (off + c) * 3
				yv := d.yTile[idx]
				cb := d.cbTile[idx]
				cr := d.crTile[idx]
				d.prevYUV[pi] = yv
				d.prevYUV[pi+1] = cb
				d.prevYUV[pi+2] = cr
				blue := d.calcRGBofY[yv] + d.calcRGBofCbToB[cb]
				green := d.calcRGBofY[yv] + d.calcRGBofCbToG[cb] + d.calcRGBofCrToG[cr]
				red := d.calcRGBofY[yv] + d.calcRGBofCrToR[cr]
				if pi < d.realWidth*d.realH*3 {
					d.decodeBuf[pi] = clampRange(d, blue)
					d.decodeBuf[pi+1] = clampRange(d, green)
					d.decodeBuf[pi+2] = clampRange(d, red)
				}
			}
			off += d.realWidth
		}
		return
	}

	// 4:2:0 path (16×16 tile): 4 luma 8×8 blocks + one 8×8 Cb + one 8×8 Cr.
	p := 0
	for blk := 0; blk < 4; blk++ {
		for i := 0; i < 64; i++ {
			d.yTile420[blk][i] = d.yuvTile[p]
			p++
		}
	}
	for i := 0; i < 64; i++ {
		d.cbTile[i] = d.yuvTile[p]
		d.crTile[i] = d.yuvTile[p+64]
		p++
	}
	x0 := tx * 16
	y0 := ty * 16
	// Faithful to Decoder.java: the 420 path seeds the offset with the padded
	// WIDTH (`y0*d.width`) but advances rows by the unpadded RealWIDTH
	// (`off += d.realWidth` below). The asymmetry only matters when width isn't a
	// multiple of 16; do not "normalize" it — it must match the reference codec.
	off := y0*d.width + x0
	var ic [4]int
	rows := 16
	if d.height == 608 && ty == 37 {
		rows = 8 // Decoder.java special case
	}
	for r := 0; r < rows; r++ {
		blkRow := (r >> 3) * 2
		yRow := (r >> 1) << 3
		for c := 0; c < 16; c++ {
			blk := blkRow + (c >> 3)
			pos := ic[blk]
			ic[blk]++
			pi := (off + c) * 3
			chroma := yRow + (c >> 1)
			yv := d.yTile420[blk][pos]
			cb := d.cbTile[chroma]
			cr := d.crTile[chroma]
			blue := d.calcRGBofY[yv] + d.calcRGBofCbToB[cb]
			green := d.calcRGBofY[yv] + d.calcRGBofCbToG[cb] + d.calcRGBofCrToG[cr]
			red := d.calcRGBofY[yv] + d.calcRGBofCrToR[cr]
			if blue >= 0 {
				d.decodeBuf[pi] = d.rangeLimitTable[blue+256]
			} else {
				d.decodeBuf[pi] = 0
			}
			if green >= 0 {
				d.decodeBuf[pi+1] = d.rangeLimitTable[green+256]
			} else {
				d.decodeBuf[pi+1] = 0
			}
			if red >= 0 {
				d.decodeBuf[pi+2] = d.rangeLimitTable[red+256]
			} else {
				d.decodeBuf[pi+2] = 0
			}
		}
		off += d.realWidth
	}
}

// clampRange replicates the 444-path clamp: if v>=0 use rangeLimitTable[v+256]
// else 0.
func clampRange(d *Decoder, v int32) byte {
	if v >= 0 {
		return d.rangeLimitTable[v+256]
	}
	return 0
}

// convertYUVToRGBPass2 ports Decoder.convertYUVToRGBPass2(txb, tyb). Pass-2 is a
// delta refinement over the previous YUV (444 only; the 420 branch in Java just
// logs and does nothing).
func (d *Decoder) convertYUVToRGBPass2(tx, ty int) {
	if d.mode420 != 0 {
		// Java prints "Receive Pass 2 data in YUV420 mode" and returns.
		return
	}
	copy(d.yTile[:], d.yuvTile[0:64])
	for i := 0; i < 64; i++ {
		d.cbTile[i] = d.yuvTile[64+i]
		d.crTile[i] = d.yuvTile[128+i]
	}
	x0 := tx * 8
	y0 := ty * 8
	off := y0*d.realWidth + x0
	limit := d.realWidth - x0
	if limit == 0 || limit > 8 {
		limit = 8
	}
	for r := 0; r < 8; r++ {
		for c := 0; c < limit; c++ {
			idx := (r << 3) + c
			pi := (off + c) * 3
			yv := d.prevYUV[pi] + (d.yTile[idx] - 128)
			cb := d.prevYUV[pi+1] + (d.cbTile[idx] - 128)
			cr := d.prevYUV[pi+2] + (d.crTile[idx] - 128)
			yv = clamp255(yv)
			cb = clamp255(cb)
			cr = clamp255(cr)
			blue := d.calcRGBofY[yv] + d.calcRGBofCbToB[cb]
			green := d.calcRGBofY[yv] + d.calcRGBofCbToG[cb] + d.calcRGBofCrToG[cr]
			red := d.calcRGBofY[yv] + d.calcRGBofCrToR[cr]
			if pi < d.realWidth*d.realH*3 {
				d.decodeBuf[pi] = clampRange(d, blue)
				d.decodeBuf[pi+1] = clampRange(d, green)
				d.decodeBuf[pi+2] = clampRange(d, red)
			}
		}
		off += d.realWidth
	}
}

func clamp255(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}
