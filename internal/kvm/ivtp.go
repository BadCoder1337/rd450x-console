package kvm

import (
	"encoding/binary"
	"io"
)

// HeaderSize is the fixed size of an IVTP packet header (little-endian):
//
//	type   uint16   (offset 0)
//	size   uint32   (offset 2) — payload bytes following the header
//	status uint16   (offset 6)
const HeaderSize = 8

// IVTP opcodes (the subset this client uses). See docs/kvm-protocol.md.
const (
	opHIDPkt             uint16 = 1
	opSetBandwidth       uint16 = 2
	opSetFPS             uint16 = 3
	opPauseRedirection   uint16 = 4
	opRefreshVideo       uint16 = 5
	opResumeRedirection  uint16 = 6
	opSetCompression     uint16 = 7
	opStopSession        uint16 = 8
	opBlankScreen        uint16 = 9
	opEnableEncryption   uint16 = 12
	opDisableEncryption  uint16 = 13
	opEncryptionStatus   uint16 = 14
	opInitialEncryption  uint16 = 15
	opValidateVideo      uint16 = 18
	opValidateVideoResp  uint16 = 19
	opGetKeybdLED        uint16 = 20
	opSessionAccepted    uint16 = 23
	opMediaState         uint16 = 24
	opVideoFragment      uint16 = 25
	opSetMouseMode       uint16 = 28
	opPowerStatus        uint16 = 34
	opPowerControl       uint16 = 35
	opGetUserMacro       uint16 = 40
	opKeepAlive          uint16 = 57
	opConnectionComplete uint16 = 58
)

// VideoPacketSize is the fixed body length of a VALIDATE_VIDEO_SESSION packet.
const VideoPacketSize = 373

// header is an 8-byte IVTP packet header.
type header struct {
	Type   uint16
	Size   uint32
	Status uint16
}

func (h header) marshal() []byte {
	b := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint16(b[0:], h.Type)
	binary.LittleEndian.PutUint32(b[2:], h.Size)
	binary.LittleEndian.PutUint16(b[6:], h.Status)
	return b
}

func readHeader(r io.Reader) (header, error) {
	var b [HeaderSize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return header{}, err
	}
	return header{
		Type:   binary.LittleEndian.Uint16(b[0:]),
		Size:   binary.LittleEndian.Uint32(b[2:]),
		Status: binary.LittleEndian.Uint16(b[6:]),
	}, nil
}

// putFixed writes s into dst as a fixed-width, zero-padded ASCII field.
// It writes at most len(dst) bytes and zero-fills the remainder.
func putFixed(dst []byte, s string) {
	n := copy(dst, s)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}
