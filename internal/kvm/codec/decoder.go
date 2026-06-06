// Package codec decodes the AMI/ASPEED KVM video stream.
//
// The wire format is the ASPEED AST hardware video engine output: a tile-based
// hybrid of VQ (vector quantization) and JPEG (DCT + Huffman + quantization),
// YUV 4:2:0, with optional RC4 encryption of the entropy-coded data and delta
// (skip) tiles relative to the previous frame.
//
// This is a clean-room port of JViewer-SOC's soc/video/Decoder.java; see
// docs/kvm-protocol.md. The Java entry point Decoder.decode(VideoEngineInfo,
// int[]) maps to (*Decoder).decode here, and the public Decode() wraps it with
// the frame-header parse that SOCFrameHdr/VideoHeader/SOCJVVideo.MakeIntArray
// performed in Java.
package codec

import (
	"errors"
	"fmt"
)

// ErrNotImplemented is retained for API compatibility; no longer returned.
var ErrNotImplemented = errors.New("kvm/codec: ASPEED decoder not yet implemented")

// virtAdd ports SOCIVTPPktHdr.VIRTADD (0 on this build).
const virtAdd = 0

// Frame is a decoded video frame, 32-bpp RGBX (little-endian), len(Pix)=W*H*4.
// RGBX matches the pixel format noVNC requests via SetPixelFormat (red-shift 0,
// green-shift 8, blue-shift 16), so the RFB raw blit lands directly in the canvas.
type Frame struct {
	W, H int
	Pix  []byte
}

// Decoder holds state carried across frames: the quantization/Huffman tables,
// the previous frame YUV (for pass-2 delta tiles), the RC4 keystream state, the
// 24-bit BGR decode buffer, and the current resolution.
//
// Field names follow Decoder.java where practical.
type Decoder struct {
	// Resolution / geometry (Decoder.decode).
	width, height       int // WIDTH/HEIGHT (padded to MB grid)
	realWidth, realH    int // RealWIDTH/RealHEIGHT (true resolution)
	tmpWidthBy16        int // tmp_WIDTHBy16 (dest X, padded)
	tmpHeightBy16       int // tmp_HEIGHTBy16 (dest Y, padded)
	mode420             int // m_Mode420

	// Per-frame quant selectors (Decoder.decode sets these).
	scaleFactor, scaleFactorUV               int // SCALEFACTOR / SCALEFACTORUV (always 16)
	advanceScaleFactor, advanceScaleFactorUV int // ADVANCESCALEFACTOR(UV)
	selector, advanceSelector, mapping       int // selector / advance_selector / Mapping

	// Quantization tables m_QT[4][64], pre-scaled by 65536 (fixed point).
	qt [4][64]int64

	// Huffman tables m_HTDC[4]/m_HTAC[4].
	htDC, htAC [4]*huffmanTable

	// Per-component DC predictors (Decoder.m_DCY/DCCb/DCCr, length-1 arrays).
	dcY, dcCb, dcCr int16

	// Component table selectors (constants in Java).
	yDCnr, cbDCnr, crDCnr byte
	yACnr, cbACnr, crACnr byte

	// Bit-reader state (Decoder.m_RecvBuffer + registers).
	recv    []uint32 // m_RecvBuffer; [0],[1] are working registers, rest is the stream
	index   int      // _index
	newbits int      // m_newbits

	// Scratch decode buffers (sized once per resolution).
	dctCoeff   [384]int32 // m_DCT_coeff
	workspace  [64]int32  // workspace (IDCT column pass)
	yuvTile    [768]int32 // YUVTile (Y(4×64) + Cb(64) + Cr(64) in 420)
	yTile420   [4][64]int32
	cbTile     [64]int32
	crTile     [64]int32
	yTile      [64]int32 // YValueInTile (444 path)

	// Output framebuffer: 24-bit BGR, 3 bytes/pixel (Decoder.m_decodeBuf).
	decodeBuf []byte
	bufStride int // pixel stride used to address decodeBuf (width in 420, realWidth in 444)
	bufAlloc  int // current decodeBuf allocation in pixels (for reuse across frames)
	// Previous-frame YUV, 3 ints/pixel (Decoder.previousYUVData).
	prevYUV []int32

	// Color / range-limit tables (built once in New()).
	rangeLimitTable      [1408]byte
	rangeLimitTableShort [1408]int16
	calcRGBofY           [256]int32
	calcRGBofCrToR       [256]int32
	calcRGBofCbToB       [256]int32
	calcRGBofCrToG       [256]int32
	calcRGBofCbToG       [256]int32
	negPow2              [17]int16

	// RC4 state (Decoder.s + DecodeRC4State).
	rc4         rc4State
	rc4SetupDone bool // DecodeRC4State

	// VQ color cache (Decoder.m_VQ / COLOR_CACHE).
	vq colorCache

	// Block placement counters (Decoder.txb/tyb).
	txb, tyb int

	// signed scratch used by getKbits.
	signedWordvalue int16
}

// colorCache mirrors Decoder.COLOR_CACHE.
type colorCache struct {
	Color      [4]uint32
	Index      [4]int
	BitMapBits byte
}

// New returns a decoder with freshly initialized tables (Decoder constructor).
func New() *Decoder {
	d := &Decoder{}
	for i := 1; i < 17; i++ {
		// neg_pow2[i] = (short)(1 - 2^i)
		d.negPow2[i] = int16(1 - (1 << uint(i)))
	}
	d.htDC, d.htAC = initHuffmanTables()
	d.initColorTable()
	d.initRangeLimitTable()
	d.precalculateCrCbTables() // builds the green tables (m_Cr_Cb_green_tab analog folded into calcRGB)

	// Component selectors (Decoder field initializers).
	d.yDCnr, d.cbDCnr, d.crDCnr = 0, 1, 1
	d.yACnr, d.cbACnr, d.crACnr = 0, 1, 1
	d.mode420 = 1
	return d
}

// Size reports the last decoded frame resolution (0,0 until the first frame).
func (d *Decoder) Size() (w, h int) { return d.realWidth, d.realH }

// Decode turns one reassembled, fragment-stripped frame buffer into a Frame.
// The buffer begins with the 86-byte VideoHeader followed by the compressed
// tile data. This wraps the Java pipeline:
//
//	VideoHeader.set()      -> parseFrameHeader (header + compressed payload)
//	SOCJVVideo.MakeIntArray -> makeIntArray (pack bytes LE into uint32[])
//	Decoder.decode()       -> (*Decoder).decode
//
// and then packs the 24-bit BGR decodeBuf into 32-bpp BGRX.
func (d *Decoder) Decode(frame []byte) (*Frame, error) {
	h, compressed, err := parseFrameHeader(frame)
	if err != nil {
		return nil, err
	}
	if h.destX <= 0 || h.destY <= 0 || h.destX > maxResolution || h.destY > maxResolution {
		return nil, fmt.Errorf("kvm/codec: bad resolution %dx%d", h.destX, h.destY)
	}

	recv := makeIntArray(compressed)
	d.decode(&h, recv)

	out := &Frame{W: d.realWidth, H: d.realH}
	out.Pix = make([]byte, d.realWidth*d.realH*4)
	// decodeBuf is 24-bit BGR (3 bytes/pixel, [B,G,R]); expand to RGBX so the
	// channel order matches the format noVNC negotiates (red byte first).
	src := d.decodeBuf
	di := 0
	for y := 0; y < d.realH; y++ {
		row := y * d.bufStride * 3
		for x := 0; x < d.realWidth; x++ {
			si := row + x*3
			out.Pix[di+0] = src[si+2] // R
			out.Pix[di+1] = src[si+1] // G
			out.Pix[di+2] = src[si+0] // B
			out.Pix[di+3] = 0         // X
			di += 4
		}
	}
	return out, nil
}

const maxResolution = 1500 // Decoder.MAX_X/Y_RESOLUTION

// makeIntArray ports SOCJVVideo.MakeIntArray: pad to a multiple of 4 bytes,
// then pack little-endian into uint32 words.
func makeIntArray(b []byte) []uint32 {
	n := len(b)
	pad := (4 - n%4) % 4
	total := n + pad
	out := make([]uint32, total/4)
	for i := 0; i < len(out); i++ {
		j := i * 4
		var v uint32
		v |= uint32(b[j+0])
		if j+1 < n {
			v |= uint32(b[j+1]) << 8
		}
		if j+2 < n {
			v |= uint32(b[j+2]) << 16
		}
		if j+3 < n {
			v |= uint32(b[j+3]) << 24
		}
		out[i] = v
	}
	return out
}

// initColorTable ports Decoder.initColorTable.
func (d *Decoder) initColorTable() {
	half := 65536 >> 1
	fixG := func(v float64) int { return int(v*65536.0 + 0.5) }
	cr := -128
	for i := 0; i < 256; i++ {
		d.calcRGBofCrToR[i] = int32((fixG(1.597656)*cr + half) >> 16)
		d.calcRGBofCbToB[i] = int32((fixG(2.015625)*cr + half) >> 16)
		d.calcRGBofCrToG[i] = int32((-fixG(0.8125)*cr + half) >> 16)
		d.calcRGBofCbToG[i] = int32((-fixG(0.390625)*cr + half) >> 16)
		cr++
	}
	yv := -16
	for i := 0; i < 256; i++ {
		d.calcRGBofY[i] = int32((fixG(1.164)*yv + half) >> 16)
		yv++
	}
}

// initRangeLimitTable ports Decoder.initRangeLimitTable.
func (d *Decoder) initRangeLimitTable() {
	for i := 0; i < 255; i++ {
		d.rangeLimitTable[i] = 0
	}
	for s := 0; s < 256; s++ {
		d.rangeLimitTable[256+s] = byte(s)
		d.rangeLimitTableShort[256+s] = int16(s)
	}
	for i := 512; i < 895; i++ {
		d.rangeLimitTable[i] = 0xFF // (byte)-1
		d.rangeLimitTableShort[i] = 255
	}
	for i := 896; i < 1279; i++ {
		d.rangeLimitTable[i] = 0
		d.rangeLimitTableShort[i] = 0
	}
	for s := 1280; s < 1408; s++ {
		d.rangeLimitTable[s] = byte(s)
		d.rangeLimitTableShort[s] = int16(s & 255)
	}
}

// precalculateCrCbTables ports Decoder.precalculateCrCbTables. The m_Cr_tab /
// m_Cb_tab / m_Cr_Cb_green_tab arrays it builds are unused by the actual
// YUV→RGB path (which uses the calcRGBof* tables from initColorTable); the Java
// keeps them as dead initialization. We retain the entry point as a no-op to
// preserve the constructor mapping. See Decoder.java lines 858-894.
func (d *Decoder) precalculateCrCbTables() {}
