package codec

// quant.go — port of the quantization-table builders in Decoder.java:
// setQuantizationTable, loadLuminanceQuantizationTable,
// loadChrominanceQuantizationTable, loadPass2Luminance/ChrominanceQuantizationTable.

// aanScaleFactor is the {1.0, 1.387..., ...} array used to fold the AAN IDCT
// scaling into the quant table (same literal floats as the Java source).
var aanScaleFactor = [8]float64{
	1.0, 1.3870399, 1.306563, 1.1758755, 1.0, 0.78569496, 0.5411961, 0.27589938,
}

// setQuantizationTable ports Decoder.setQuantizationTable. src is a signed Java
// byte table; scale is SCALEFACTOR (16). out is written in de-zigzag order.
func setQuantizationTable(src *[64]int8, scale int, out *[64]byte) {
	for i := 0; i < 64; i++ {
		// i = (src[i] * 16) / scale, with signed-byte arithmetic.
		v := (int(src[i]) * 16) / scale
		if v <= 0 {
			v = 1
		}
		if v > 255 {
			v = 255
		}
		out[zigzag[i]] = byte(v)
	}
}

// applyAANScale folds the AAN scale factors and the 65536 fixed-point shift into
// the quant table, mirroring the inner double loop shared by all four loaders.
func applyAANScale(qt *[64]int64) {
	idx := 0
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			qt[idx] = int64(float64(qt[idx])*aanScaleFactor[r]*aanScaleFactor[c]) * 65536
			idx++
		}
	}
}

// loadLuminanceQuantizationTable ports Decoder.loadLuminanceQuantizationTable.
func (d *Decoder) loadLuminanceQuantizationTable(qt *[64]int64) {
	var tmp [64]byte
	src := tblY[d.selector]
	setQuantizationTable(&src, d.scaleFactor, &tmp)
	for i := 0; i <= 63; i++ {
		qt[i] = int64(uint8(tmp[zigzag[i]]))
	}
	applyAANScale(qt)
}

// loadChrominanceQuantizationTable ports Decoder.loadChrominanceQuantizationTable.
func (d *Decoder) loadChrominanceQuantizationTable(qt *[64]int64) {
	var tmp [64]byte
	var src [64]int8
	if d.mapping == 1 {
		src = tblY[d.selector]
	} else {
		src = tblUV[d.selector]
	}
	setQuantizationTable(&src, d.scaleFactorUV, &tmp)
	for i := 0; i <= 63; i++ {
		qt[i] = int64(uint8(tmp[zigzag[i]]))
	}
	applyAANScale(qt)
}

// loadPass2LuminanceQuantizationTable ports
// Decoder.loadPass2LuminanceQuantizationTable.
func (d *Decoder) loadPass2LuminanceQuantizationTable(qt *[64]int64) {
	var tmp [64]byte
	src := tblY[d.advanceSelector]
	setQuantizationTable(&src, d.advanceScaleFactor, &tmp)
	for i := 0; i <= 63; i++ {
		qt[i] = int64(uint8(tmp[zigzag[i]]))
	}
	applyAANScale(qt)
}

// loadPass2ChrominanceQuantizationTable ports
// Decoder.loadPass2ChrominanceQuantizationTable. NOTE: unlike the other three,
// the Java pass-2 chrominance loader reads the *unscaled-by-zigzag* bArr
// directly (jArr[b2] = getShort(bArr[zigzag[b2]])) — i.e. it dequantizes from
// the tmp buffer rather than from a separate quantizationTable copy. The data
// is identical because setQuantizationTable also writes into bArr. (Decoder.java
// lines 610-616.)
func (d *Decoder) loadPass2ChrominanceQuantizationTable(qt *[64]int64) {
	var tmp [64]byte
	var src [64]int8
	if d.mapping == 1 {
		src = tblY[d.advanceSelector]
	} else {
		src = tblUV[d.advanceSelector]
	}
	setQuantizationTable(&src, d.advanceScaleFactorUV, &tmp)
	for i := 0; i <= 63; i++ {
		qt[i] = int64(uint8(tmp[zigzag[i]]))
	}
	applyAANScale(qt)
}
