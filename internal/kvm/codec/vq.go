package codec

// vq.go — port of Decoder.decompressVQ. The VQ path fills a 16×16 (or 8×8 in
// 444) tile from a 1/2/4-entry 24-bit colour cache, optionally reading a
// per-pixel index of BitMapBits bits, then runs the same YUV→RGB conversion.

// decompressVQ ports Decoder.decompressVQ(txb, tyb, b). The colour cache holds
// packed 0xRRGGBB where R is the Y component, G is Cb, B is Cr (the AST VQ
// stores YCbCr in the 24-bit word). It writes Y/Cb/Cr planes into YUVTile then
// converts.
func (d *Decoder) decompressVQ(tx, ty int) {
	idx := 0
	if d.vq.BitMapBits == 0 {
		color := d.vq.Color[d.vq.Index[0]]
		yv := int32((color & 0x00FF0000) >> 16)
		cb := int32((color & 0x0000FF00) >> 8)
		cr := int32(color & 0x000000FF)
		for i := 0; i < 64; i++ {
			d.yuvTile[idx+0] = yv
			d.yuvTile[idx+64] = cb
			d.yuvTile[idx+128] = cr
			idx++
		}
	} else {
		bits := int(d.vq.BitMapBits)
		for i := 0; i < 64; i++ {
			sel := int(d.lookKbits(bits))
			color := d.vq.Color[d.vq.Index[sel]]
			d.yuvTile[idx+0] = int32((color & 0x00FF0000) >> 16)
			d.yuvTile[idx+64] = int32((color & 0x0000FF00) >> 8)
			d.yuvTile[idx+128] = int32(color & 0x000000FF)
			idx++
			d.skipKbits(bits)
		}
	}
	d.convertYUVtoRGB(tx, ty)
}
