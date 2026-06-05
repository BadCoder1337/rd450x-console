package codec

// idct.go — port of Decoder.decodeHuffmanDataUnit (entropy decode into
// m_DCT_coeff) and Decoder.inverseDCT (the AAN-style integer IDCT). Do not
// substitute a different IDCT: the constants 277/362/473/669 and the >>8
// MULTIPLY are the AST firmware's exact transform.

// multiply ports Decoder.MULTIPLY: (a*b) >> 8.
func multiply(a, b int32) int32 { return (a * b) >> 8 }

// decodeHuffmanDataUnit ports Decoder.decodeHuffmanDataUnit(dcTbl, acTbl, dcPred, off).
// dcPred is the running DC predictor for the component (m_DCY/DCCb/DCCr[0]).
// off is the base offset into m_DCT_coeff (s). Returns the updated predictor.
func (d *Decoder) decodeHuffmanDataUnit(dcTbl, acTbl byte, dcPred int16, off int) int16 {
	// Arrays.fill(m_DCT_coeff, 0)
	for i := range d.dctCoeff {
		d.dctCoeff[i] = 0
	}

	htDC := d.htDC[dcTbl]
	// b3 = Len[(recv[0]>>16)&0xFFFF]
	b3 := int(htDC.Len[(d.recv[0]>>16)&0xFFFF])
	look := d.lookKbits(b3)
	d.skipKbits(b3)
	// value index = WORD_hi_lo(b3, look - minor_code[b3])
	idx := getInt16(wordHiLo(int8(b3), int8(int(look)-int(htDC.minorCode[b3]))))
	b4 := int8(htDC.V[idx]) // magnitude category
	if b4 == 0 {
		d.dctCoeff[off+0] = int32(dcPred)
	} else {
		d.dctCoeff[off] = int32(dcPred) + int32(d.getKbits(int(b4)))
		dcPred = int16(d.dctCoeff[off])
	}

	htAC := d.htAC[acTbl]
	b5 := 1
	for {
		b6 := int(htAC.Len[(d.recv[0]>>16)&0xFFFF])
		look2 := d.lookKbits(b6)
		d.skipKbits(b6)
		b7 := int8(htAC.V[getInt16(wordHiLo(int8(b6), int8(int(look2)-int(htAC.minorCode[b6]))))])
		b8 := int(b7 & 15)        // run/size: low nibble = magnitude bits
		b9 := int((b7 >> 4) & 15) // high nibble = zero run
		if b8 == 0 {
			if b9 == 15 {
				b5 += 16
			} else {
				return dcPred
			}
		} else {
			b10 := b5 + b9
			d.dctCoeff[off+int(dezigzag[b10])] = int32(d.getKbits(b8))
			b5 = b10 + 1
		}
		if b5 >= 64 {
			break
		}
	}
	return dcPred
}

// inverseDCT ports Decoder.inverseDCT(off, qtIdx). It dequantizes m_DCT_coeff
// using m_QT[qtIdx] and writes the spatial result into YUVTile at off.
func (d *Decoder) inverseDCT(off int, qtIdx byte) {
	qt := &d.qt[qtIdx]
	c := &d.dctCoeff
	ws := &d.workspace

	// Column pass.
	i4 := off
	i2 := 0
	i3 := 0
	for col := 8; col > 0; col-- {
		if (c[i4+8] | c[i4+16] | c[i4+24] | c[i4+32] | c[i4+40] | c[i4+48] | c[i4+56]) == 0 {
			dc := int32((int64(c[i4+0]) * qt[i2+0]) >> 16)
			ws[i3+0] = dc
			ws[i3+8] = dc
			ws[i3+16] = dc
			ws[i3+24] = dc
			ws[i3+32] = dc
			ws[i3+40] = dc
			ws[i3+48] = dc
			ws[i3+56] = dc
			i4++
			i2++
			i3++
			continue
		}
		t0 := int32((int64(c[i4+0]) * qt[i2+0]) >> 16)
		t1 := int32((int64(c[i4+16]) * qt[i2+16]) >> 16)
		t2 := int32((int64(c[i4+32]) * qt[i2+32]) >> 16)
		t3 := int32((int64(c[i4+48]) * qt[i2+48]) >> 16)
		tmp10 := t0 + t2
		tmp11 := t0 - t2
		tmp13 := t1 + t3
		tmp12 := multiply(t1-t3, 362) - tmp13
		a0 := tmp10 + tmp13
		a3 := tmp10 - tmp13
		a1 := tmp11 + tmp12
		a2 := tmp11 - tmp12

		s0 := int32((int64(c[i4+8]) * qt[i2+8]) >> 16)
		s1 := int32((int64(c[i4+24]) * qt[i2+24]) >> 16)
		s2 := int32((int64(c[i4+40]) * qt[i2+40]) >> 16)
		s3 := int32((int64(c[i4+56]) * qt[i2+56]) >> 16)
		z13 := s2 + s1
		z10 := s2 - s1
		z11 := s0 + s3
		z12 := s0 - s3
		u7 := z11 + z13
		u11 := multiply(z11-z13, 362)
		z5 := multiply(z10+z12, 473)
		u10 := multiply(z12, 277) - z5
		u12 := (multiply(z10, -669) + z5) - u7
		v6 := u11 - u12
		v5 := u10 + v6

		ws[i3+0] = a0 + u7
		ws[i3+56] = a0 - u7
		ws[i3+8] = a1 + u12
		ws[i3+48] = a1 - u12
		ws[i3+16] = a2 + v6
		ws[i3+40] = a2 - v6
		ws[i3+32] = a3 + v5
		ws[i3+24] = a3 - v5
		i4++
		i2++
		i3++
	}

	// Row pass.
	w := 0
	for r := 0; r < 8; r++ {
		base := off + r*8
		tmp10 := ws[w+0] + ws[w+4]
		tmp11 := ws[w+0] - ws[w+4]
		tmp13 := ws[w+2] + ws[w+6]
		tmp12 := multiply(ws[w+2]-ws[w+6], 362) - tmp13
		a0 := tmp10 + tmp13
		a3 := tmp10 - tmp13
		a1 := tmp11 + tmp12
		a2 := tmp11 - tmp12

		z13 := ws[w+5] + ws[w+3]
		z10 := ws[w+5] - ws[w+3]
		z11 := ws[w+1] + ws[w+7]
		z12 := ws[w+1] - ws[w+7]
		u7 := z11 + z13
		u11 := multiply(z11-z13, 362)
		z5 := multiply(z10+z12, 473)
		u10 := multiply(z12, 277) - z5
		u12 := (multiply(z10, -669) + z5) - u7
		v6 := u11 - u12
		v5 := u10 + v6

		d.yuvTile[base+0] = int32(d.rangeLimitTableShort[128+(((a0+u7)>>3)&1023)+256])
		d.yuvTile[base+7] = int32(d.rangeLimitTableShort[128+(((a0-u7)>>3)&1023)+256])
		d.yuvTile[base+1] = int32(d.rangeLimitTableShort[128+(((a1+u12)>>3)&1023)+256])
		d.yuvTile[base+6] = int32(d.rangeLimitTableShort[128+(((a1-u12)>>3)&1023)+256])
		d.yuvTile[base+2] = int32(d.rangeLimitTableShort[128+(((a2+v6)>>3)&1023)+256])
		d.yuvTile[base+5] = int32(d.rangeLimitTableShort[128+(((a2-v6)>>3)&1023)+256])
		d.yuvTile[base+4] = int32(d.rangeLimitTableShort[128+(((a3+v5)>>3)&1023)+256])
		d.yuvTile[base+3] = int32(d.rangeLimitTableShort[128+(((a3-v5)>>3)&1023)+256])
		w += 8
	}
}
