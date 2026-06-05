package codec

import (
	"encoding/binary"
	"fmt"
)

// header.go — port of soc/video/VideoHeader.java (the 86-byte frame header)
// and the subset of soc/SOCFrameHdr.java + VideoEngineInfo that the decoder
// reads. The reassembled frame buffer is: [86-byte VideoHeader][compressed
// tile data]. VideoHeader.set() returns the bytes after offset 86 as the
// compressed buffer.

// videoHeaderSize is VideoHeader.VIDEO_HDR_SIZE.
const videoHeaderSize = 86

// frameHeader carries the fields the decoder consumes, named after
// VideoEngineInfo.FRAME_HEADER / SourceModeInfo / DestinationModeInfo.
type frameHeader struct {
	// Source/destination resolutions.
	sourceX, sourceY int // SourceModeInfo.X/.Y
	destX, destY     int // DestinationModeInfo.X/.Y (== width/height used for placement)

	// FRAME_HEADER selectors.
	compressionMode      int
	jpegScaleFactor      int
	jpegTableSelector    int // selector
	jpegYUVTableMapping  int // Mapping
	sharpModeSelection   int
	advanceTableSelector int // advance_selector
	advanceScaleFactor   int
	numberOfMB           int
	rc4Enable            int
	rc4Reset             int
	mode420              int

	compressSize int // CompressData.CompressSize
}

// parseFrameHeader ports VideoHeader.set() (little-endian field reads) followed
// by SOCFrameHdr.getFrameVariables(). It returns the parsed header and the
// compressed payload (the bytes after the 86-byte header).
func parseFrameHeader(frame []byte) (frameHeader, []byte, error) {
	if len(frame) < videoHeaderSize {
		return frameHeader{}, nil, fmt.Errorf("kvm/codec: frame too short: %d < %d", len(frame), videoHeaderSize)
	}
	le := binary.LittleEndian
	b := frame

	// VideoHeader.set field order (all little-endian):
	//  0  iEngVersion          short
	//  2  wHeaderLen           short
	//  4  SourceMode_X         short
	//  6  SourceMode_Y         short
	//  8  SourceMode_ColorDepth   short
	// 10  SourceMode_RefreshRate  short
	// 12  SourceMode_ModeIndex    byte
	// 13  DestinationMode_X    short
	// 15  DestinationMode_Y    short
	// 17  DestinationMode_ColorDepth short
	// 19  DestinationMode_RefreshRate short
	// 21  DestinationMode_ModeIndex byte
	// 22  FrameHdr_StartCode   int
	// 26  FrameHdr_FrameNumber int
	// 30  FrameHdr_HSize       short
	// 32  FrameHdr_VSize       short
	// 34  FrameHdr_Reserved[0] int
	// 38  FrameHdr_Reserved[1] int
	// 42  FrameHdr_CompressionMode  byte
	// 43  FrameHdr_JPEGScaleFactor  byte
	// 44  FrameHdr_JPEGTableSelector byte
	// 45  FrameHdr_JPEGYUVTableMapping byte
	// 46  FrameHdr_SharpModeSelection  byte
	// 47  FrameHdr_AdvanceTableSelector byte
	// 48  FrameHdr_AdvanceScaleFactor  byte
	// 49  FrameHdr_NumberOfMB  int
	// 53  FrameHdr_RC4Enable   byte
	// 54  FrameHdr_RC4Reset    byte
	// 55  Mode420              byte
	// 56  InfData_DownScalingMethod byte
	// 57  InfData_DifferentialSetting byte
	// 58  InfData_AnalogDifferentialThreshold short
	// 60  InfData_DigitalDifferentialThreshold short
	// 62  InfData_ExternalSignalEnable byte
	// 63  InfData_AutoMode     byte
	// 64  InfData_VQMode       byte
	// 65  CompressData_SourceFrameSize int
	// 69  CompressData_CompressSize    int
	// 73  CompressData_HDebug  int
	// 77  CompressData_VDebug  int
	// 81  InputSignal          byte
	// 82  Cursor_XPos          short
	// 84  Cursor_YPos          short
	var h frameHeader
	h.sourceX = int(le.Uint16(b[4:]))
	h.sourceY = int(le.Uint16(b[6:]))
	h.destX = int(le.Uint16(b[13:]))
	h.destY = int(le.Uint16(b[15:]))
	h.compressionMode = int(b[42])
	h.jpegScaleFactor = int(b[43])
	h.jpegTableSelector = int(b[44])
	h.jpegYUVTableMapping = int(b[45])
	h.sharpModeSelection = int(b[46])
	h.advanceTableSelector = int(b[47])
	h.advanceScaleFactor = int(b[48])
	h.numberOfMB = int(int32(le.Uint32(b[49:])))
	h.rc4Enable = int(b[53])
	h.rc4Reset = int(b[54])
	h.mode420 = int(b[55])
	h.compressSize = int(int32(le.Uint32(b[69:])))

	compressed := frame[videoHeaderSize:]
	return h, compressed, nil
}
