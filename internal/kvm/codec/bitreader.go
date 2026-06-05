package codec

// bitreader.go — port of the bit-level reader in Decoder.java: lookKbits,
// skipKbits, getKbits, updateReadBuf. The stream lives in d.recv as packed
// little-endian uint32 words; d.recv[0]/[1] are the two working registers and
// d.index points at the next refill word. d.newbits tracks how many valid bits
// remain in register[1]. All arithmetic uses unsigned 32-bit semantics to match
// Java's getLong()=(x & 0xFFFFFFFF).

// lookKbits ports Decoder.lookKbits: top b bits of register0.
func (d *Decoder) lookKbits(b int) int16 {
	return int16(d.recv[0] >> uint(32-b))
}

// skipKbits ports Decoder.skipKbits.
func (d *Decoder) skipKbits(b int) {
	if d.newbits-b <= 0 {
		if virtAdd+d.index > len(d.recv)-1 {
			d.index = (len(d.recv) - 1) - virtAdd
		}
		r0 := d.recv[0]
		r1 := d.recv[1]
		refill := d.recv[virtAdd+d.index]
		d.recv[0] = (r0 << uint(b)) | uint32((uint64(r1)|(uint64(refill)>>uint(d.newbits)))>>uint(32-b))
		d.recv[1] = refill << uint(b-d.newbits)
		d.newbits = (32 + d.newbits) - b
		d.index++
		return
	}
	r0 := d.recv[0]
	r1 := d.recv[1]
	d.recv[0] = (r0 << uint(b)) | (r1 >> uint(32-b))
	d.recv[1] = r1 << uint(b)
	d.newbits -= b
}

// updateReadBuf ports Decoder.updateReadBuf (identical refill logic to
// skipKbits but saves the refill word first; behaviour is the same with
// VIRTADD=0).
func (d *Decoder) updateReadBuf(i int) {
	if d.newbits-i <= 0 {
		readbuf := uint64(d.recv[virtAdd+d.index])
		d.index++
		r0 := uint64(d.recv[0])
		r1 := uint64(d.recv[1])
		d.recv[0] = uint32((r0 << uint(i)) | ((r1 | (readbuf >> uint(d.newbits))) >> uint(32-i)))
		d.recv[1] = uint32(readbuf << uint(i-d.newbits))
		d.newbits = (32 + d.newbits) - i
		return
	}
	r0 := uint64(d.recv[0])
	r1 := uint64(d.recv[1])
	d.recv[0] = uint32((r0 << uint(i)) | (r1 >> uint(32-i)))
	d.recv[1] = uint32(r1 << uint(i))
	d.newbits -= i
}

// getKbits ports Decoder.getKbits: read b bits and sign-extend via neg_pow2.
func (d *Decoder) getKbits(b int) int16 {
	d.signedWordvalue = d.lookKbits(b)
	if (int16(1<<uint(b-1)) & d.signedWordvalue) == 0 {
		d.signedWordvalue = d.signedWordvalue + d.negPow2[b]
	}
	d.skipKbits(b)
	return d.signedWordvalue
}
