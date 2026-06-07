// Package vmedia implements the AMI MegaRAC virtual-media (IUSB) data plane:
// it presents a local disk image to the host as a USB CD/DVD (or floppy/HD) by
// answering the BMC's SCSI sector reads over the proprietary IUSB protocol.
//
// The BMC uses a client-streams-sectors model: it exposes a USB mass-storage
// device to the host and, on demand, pulls the sectors the host reads from us
// over a dedicated TLS socket (CD 5120 / FD 5122 / HD 5123). This is the opposite
// of the device-local-image model every open IP-KVM stack uses, which is why the
// data plane is necessarily ours (see docs/kvm-vmedia.md).
//
// The wire protocol was reverse-engineered from the decompiled JViewer
// `com.ami.iusb.*` sources (the SCSI *emulation* there is native, but the
// transport/handshake/framing are Java and reproduced here). See
// docs/kvm-vmedia.md for the annotated handshake and header layout.
package vmedia

import (
	"encoding/binary"
	"fmt"
)

// HeaderLen is the fixed size of an IUSB packet header.
const HeaderLen = 32

// signature is the 8-byte magic at the start of every IUSB header ("IUSB" + 4 spaces).
const signature = "IUSB    "

const (
	iusbMajor = 1
	iusbMinor = 0
)

// Device types (IUSBHeader.deviceType). Only CD-ROM is confirmed from the RE;
// floppy/HD codes still need verification against a capture.
const (
	DeviceCDROM = 5
)

// Default per-device TLS ports on the BMC.
const (
	PortCD = 5120
	PortFD = 5122
	PortHD = 5123
)

// Opcodes carried in the SCSI payload at offset opcodeOffset. The 0xF* values
// are AMI's redirection-control opcodes; 0x1B is the standard SCSI START STOP
// UNIT (used by the host to eject).
const (
	OpAuth          = 0xF2 // client→server: session-token authentication
	OpRedirectAck   = 0xF1 // server→client: redirection acknowledgement
	OpKillRedir     = 0xF6 // server→client: terminate this redirection
	OpStartStopUnit = 0x1B // SCSI START STOP UNIT (eject when CDB[4] low nibble == 2)
)

// Offsets inside the IUSB payload (after the 32-byte header). The SCSI CDB
// begins at opcodeOffset, so opcode == CDB[0] and ejectByteOffset == CDB[4].
const (
	envOffDataLen     = 0  // payload[0:4]  = transfer/data length, u32 LE (request: bytes wanted)
	envOffRespLen     = 25 // payload[25:] = response data length the BMC forwards to the host (the field that actually counts)
	opcodeOffset      = 9  // payload[9]  = SCSI CDB byte 0 (opcode)
	ejectByteOffset   = 13 // payload[13] = SCSI CDB byte 4 (START STOP UNIT loej bits)
	connStatusOffset  = 30 // payload[30] = connectionStatus in an ACK
	authTokenOffset   = 31 // payload[31..] = web session token in an auth packet
	authPayloadLen    = 128
	ssiAuthPayloadLen = 240
)

// Connection-status values reported in an ACK's connStatusOffset byte.
const (
	connOK        = 1 // redirection accepted
	connErrInUse5 = 5
	connErrInUse8 = 8
)

// Header is a decoded IUSB packet header. Multi-byte fields are little-endian on
// the wire; DataPacketLen is the framing length (payload bytes following the
// header).
type Header struct {
	Major, Minor    uint8
	DataPacketLen   uint32
	ServerCaps      uint8
	DeviceType      uint8
	Protocol        uint8
	Direction       uint8
	DeviceNumber    uint8
	InterfaceNumber uint8
	ClientData      uint8
	Instance        uint8
	SequenceNumber  uint32
}

// marshal writes the 32-byte header into dst (which must be >= HeaderLen) and
// sets the checksum byte so the receiver's sum over the 32 header bytes is zero.
// JViewer computes the checksum over the header only (the payload is appended
// after), so we do the same — the BMC never checksums the payload.
func (h *Header) marshal(dst []byte) {
	b := dst[:HeaderLen]
	for i := range b {
		b[i] = 0
	}
	copy(b[0:8], signature)
	b[8] = h.Major
	b[9] = h.Minor
	b[10] = HeaderLen
	// b[11] checksum filled in below.
	binary.LittleEndian.PutUint32(b[12:16], h.DataPacketLen)
	b[16] = h.ServerCaps
	b[17] = h.DeviceType
	b[18] = h.Protocol
	b[19] = h.Direction
	b[20] = h.DeviceNumber
	b[21] = h.InterfaceNumber
	b[22] = h.ClientData
	b[23] = h.Instance
	binary.LittleEndian.PutUint32(b[24:28], h.SequenceNumber)
	// reserved[28:32] left zero.

	var sum byte
	for _, x := range b {
		sum += x
	}
	b[11] = -sum // (-Σ) & 0xFF; byte arithmetic wraps mod 256
}

// parseHeader decodes a 32-byte IUSB header. It validates the signature but not
// the checksum (the BMC's own auth packet proves the checksum need not cover the
// payload, and we have no reason to reject otherwise-valid frames).
func parseHeader(b []byte) (Header, error) {
	if len(b) < HeaderLen {
		return Header{}, fmt.Errorf("iusb: short header: %d bytes", len(b))
	}
	if string(b[0:8]) != signature {
		return Header{}, fmt.Errorf("iusb: bad signature %q", b[0:8])
	}
	return Header{
		Major:           b[8],
		Minor:           b[9],
		DataPacketLen:   binary.LittleEndian.Uint32(b[12:16]),
		ServerCaps:      b[16],
		DeviceType:      b[17],
		Protocol:        b[18],
		Direction:       b[19],
		DeviceNumber:    b[20],
		InterfaceNumber: b[21],
		ClientData:      b[22],
		Instance:        b[23],
		SequenceNumber:  binary.LittleEndian.Uint32(b[24:28]),
	}, nil
}

// Packet is a decoded IUSB packet: a header plus its payload (the SCSI block).
type Packet struct {
	Header  Header
	Payload []byte
}

// Opcode returns the SCSI/redirection opcode (payload[opcodeOffset]), or 0 if the
// payload is too short to carry one.
func (p *Packet) Opcode() int {
	if len(p.Payload) <= opcodeOffset {
		return 0
	}
	return int(p.Payload[opcodeOffset])
}

// IsEject reports whether this is a START STOP UNIT eject request (opcode 0x1B,
// CDB[4] low bits == 2), matching JViewer's detection.
func (p *Packet) IsEject() bool {
	return p.Opcode() == OpStartStopUnit &&
		len(p.Payload) > ejectByteOffset && p.Payload[ejectByteOffset]&0x03 == 2
}

// IsKill reports whether the BMC asked to terminate the redirection.
func (p *Packet) IsKill() bool { return p.Opcode() == OpKillRedir }
