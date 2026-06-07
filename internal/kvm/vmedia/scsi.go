package vmedia

import (
	"encoding/binary"
	"io"
	"log"
)

// Logical block sizes. CDs use 2048-byte blocks; floppy/HD/USB use 512. The host
// issues LBAs in these units.
const (
	cdBlockSize   = 2048
	diskBlockSize = 512
)

// SCSI / MMC opcodes we emulate for a read-only CD-ROM.
const (
	scsiTestUnitReady  = 0x00
	scsiRequestSense   = 0x03
	scsiInquiry        = 0x12
	scsiModeSelect6    = 0x15
	scsiModeSense6     = 0x1A
	scsiStartStopUnit  = 0x1B
	scsiPreventAllow   = 0x1E
	scsiReadCapacity10 = 0x25
	scsiRead10         = 0x28
	scsiWrite10        = 0x2A
	scsiReadTOC        = 0x43
	scsiGetConfig      = 0x46
	scsiGetEventStatus = 0x4A
	scsiReadDiscInfo   = 0x51
	scsiModeSense10    = 0x5A
	scsiRead12         = 0xA8
	scsiWrite12        = 0xAA
)

// Device emulates a read-only USB mass-storage device backed by a Reader,
// answering the BMC's IUSB-wrapped SCSI commands. It covers both profiles:
//   - CD-ROM: 2048-byte blocks, MMC command set (READ TOC / GET CONFIGURATION /
//     READ DISC INFORMATION) — NewCDROM, port 5120.
//   - Floppy / HD / USB: 512-byte blocks, plain Direct-Access (SBC) — NewDisk,
//     ports 5122 / 5123. The host enumerates these as Direct-Access and never
//     issues the MMC probes.
//
// The BMC firmware answers INQUIRY itself and forwards TEST UNIT READY (a
// media-present poll) plus the data commands; the SCSI emulation here is standard
// and spec-correct, while the IUSB response framing is in buildResponse.
type Device struct {
	r         Reader
	w         WriterAt // non-nil ⇒ writable (honours WRITE(10/12))
	blockSize int
	mmc       bool   // CD-ROM: also answer the MMC probes
	lastLBA   uint32 // highest addressable block
	nBytes    int64  // bytes served (read+written; stats)
}

// WriterAt is the write half of a writable backing (io.WriterAt-shaped).
type WriterAt interface {
	WriteAt(p []byte, off int64) (int, error)
}

// NewCDROM builds a read-only CD-ROM emulator (2048-byte blocks, MMC) over r.
func NewCDROM(r Reader) *Device { return newDevice(r, nil, cdBlockSize, true) }

// NewDisk builds a read-only Direct-Access (floppy / HD / USB) emulator (512-byte
// blocks, no MMC) over r.
func NewDisk(r Reader) *Device { return newDevice(r, nil, diskBlockSize, false) }

// NewDiskRW builds a writable Direct-Access emulator over rw, honouring the host's
// SCSI WRITE(10)/WRITE(12) by writing back through rw.WriteAt.
func NewDiskRW(rw ReadWriter) *Device { return newDevice(rw, rw, diskBlockSize, false) }

func newDevice(r Reader, w WriterAt, blockSize int, mmc bool) *Device {
	last := uint32(0)
	if n := r.Size() / int64(blockSize); n > 0 {
		last = uint32(n - 1)
	}
	return &Device{r: r, w: w, blockSize: blockSize, mmc: mmc, lastLBA: last}
}

// BytesServed reports how many image bytes have been sent to the host.
func (c *Device) BytesServed() int64 { return c.nBytes }

// Handle dispatches one IUSB SCSI request, returning the response payload.
func (c *Device) Handle(req *Packet) ([]byte, error) {
	cdb := req.cdb()
	if len(cdb) == 0 {
		return nil, nil
	}
	switch cdb[0] {
	case scsiTestUnitReady, scsiPreventAllow, scsiStartStopUnit, scsiModeSelect6:
		// Status-only commands: acknowledge with no data. (Eject — START STOP
		// UNIT with loej bits — is reported by Packet.IsEject for the loop to act
		// on; we still ack it here.)
		return c.buildResponse(req, nil), nil

	case scsiInquiry:
		return c.buildResponse(req, c.inquiry(allocLen(cdb, 3, 4))), nil

	case scsiRequestSense:
		return c.buildResponse(req, senseFixed(0, 0, 0)), nil // no sense

	case scsiReadCapacity10:
		return c.buildResponse(req, c.readCapacity()), nil

	case scsiRead10:
		lba := binary.BigEndian.Uint32(cdb[2:6])
		blocks := uint32(binary.BigEndian.Uint16(cdb[7:9]))
		return c.buildResponse(req, c.read(lba, blocks)), nil

	case scsiRead12:
		lba := binary.BigEndian.Uint32(cdb[2:6])
		blocks := binary.BigEndian.Uint32(cdb[6:10])
		return c.buildResponse(req, c.read(lba, blocks)), nil

	case scsiWrite10:
		lba := binary.BigEndian.Uint32(cdb[2:6])
		blocks := uint32(binary.BigEndian.Uint16(cdb[7:9]))
		c.write(req, lba, blocks)
		return c.buildResponse(req, nil), nil

	case scsiWrite12:
		lba := binary.BigEndian.Uint32(cdb[2:6])
		blocks := binary.BigEndian.Uint32(cdb[6:10])
		c.write(req, lba, blocks)
		return c.buildResponse(req, nil), nil

	case scsiModeSense6:
		return c.buildResponse(req, c.modeSense6()), nil

	case scsiModeSense10:
		return c.buildResponse(req, c.modeSense10()), nil

	// MMC-only (CD-ROM). Direct-Access hosts won't send these; reply only when
	// emulating a CD so a stray probe on a disk gets an empty (harmless) reply.
	case scsiReadTOC:
		if c.mmc {
			return c.buildResponse(req, c.readTOC(cdb)), nil
		}
		return c.buildResponse(req, nil), nil
	case scsiGetConfig:
		if c.mmc {
			return c.buildResponse(req, c.getConfiguration()), nil
		}
		return c.buildResponse(req, nil), nil
	case scsiGetEventStatus:
		if c.mmc {
			return c.buildResponse(req, getEventStatusNoChange(cdb)), nil
		}
		return c.buildResponse(req, nil), nil
	case scsiReadDiscInfo:
		if c.mmc {
			return c.buildResponse(req, c.readDiscInfo()), nil
		}
		return c.buildResponse(req, nil), nil

	default:
		log.Printf("vmedia: unhandled SCSI opcode 0x%02X (replying empty)", cdb[0])
		return c.buildResponse(req, nil), nil
	}
}

// buildResponse wraps SCSI response data in the IUSB response payload.
//
// Confirmed against the live BMC + host (vmedia is plaintext, so verifiable):
// the response payload echoes the request's command envelope, then appends the
// SCSI data-in bytes. The length the BMC forwards to the host is taken from the
// envelope field at envOffRespLen — setting only envOffDataLen yields a 0-length
// read (DID_ERROR on the host). The BMC firmware answers enumeration (INQUIRY)
// itself and forwards TEST UNIT READY (media-present poll) plus the data commands
// (READ(10), …). A bare reply leaves TUR pending and /dev/sr0 never appears; the
// echo-envelope reply with the response length set makes the disc appear and read
// correctly end-to-end.
func (c *Device) buildResponse(req *Packet, data []byte) []byte {
	env := req.Payload
	out := make([]byte, len(env)+len(data))
	copy(out, env)
	copy(out[len(env):], data)
	n := uint32(len(data))
	if len(out) >= envOffDataLen+4 {
		binary.LittleEndian.PutUint32(out[envOffDataLen:envOffDataLen+4], n)
	}
	if len(out) >= envOffRespLen+4 {
		binary.LittleEndian.PutUint32(out[envOffRespLen:envOffRespLen+4], n)
	}
	return out
}

// read returns blocks*blockSize bytes starting at lba, zero-filling any short
// read past end-of-medium.
func (c *Device) read(lba, blocks uint32) []byte {
	n := int(blocks) * c.blockSize
	buf := make([]byte, n)
	off := int64(lba) * int64(c.blockSize)
	got, err := c.r.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		log.Printf("vmedia: read lba=%d blocks=%d off=%d: %v", lba, blocks, off, err)
	}
	c.nBytes += int64(got)
	return buf
}

// write applies a WRITE(10)/WRITE(12): the host's data for the command rides in
// the request payload, appended after the command envelope (mirroring how READ
// responses append their data). We take the trailing blocks*blockSize bytes —
// robust to the exact envelope size — and write them at lba*blockSize.
func (c *Device) write(req *Packet, lba, blocks uint32) {
	n := int(blocks) * c.blockSize
	if n == 0 {
		return
	}
	if c.w == nil {
		log.Printf("vmedia: WRITE lba=%d blocks=%d on a read-only device — ignored", lba, blocks)
		return
	}
	if len(req.Payload) < n {
		log.Printf("vmedia: WRITE lba=%d blocks=%d: payload has %d bytes, need %d — skipping",
			lba, blocks, len(req.Payload), n)
		return
	}
	data := req.Payload[len(req.Payload)-n:] // trailing n bytes = the write data
	off := int64(lba) * int64(c.blockSize)
	wrote, err := c.w.WriteAt(data, off)
	if err != nil {
		log.Printf("vmedia: write lba=%d blocks=%d off=%d: %v", lba, blocks, off, err)
	}
	c.nBytes += int64(wrote)
}

// readCapacity returns the 8-byte READ CAPACITY(10) response: last LBA and block
// size, both big-endian.
func (c *Device) readCapacity() []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], c.lastLBA)
	binary.BigEndian.PutUint32(b[4:8], uint32(c.blockSize))
	return b
}

// inquiry returns standard INQUIRY data, trimmed to the allocation length the host
// asked for. (Rarely used: the BMC firmware answers INQUIRY itself.)
func (c *Device) inquiry(alloc int) []byte {
	b := make([]byte, 36)
	if c.mmc {
		b[0] = 0x05 // peripheral device type: CD/DVD
		copy(b[16:32], "Virtual CD-ROM  ")
	} else {
		b[0] = 0x00 // peripheral device type: Direct-Access (floppy/HD/USB)
		copy(b[16:32], "Virtual Disk    ")
	}
	b[1] = 0x80               // RMB: removable
	b[2] = 0x05               // version: SPC-3
	b[3] = 0x02               // response data format
	b[4] = 31                 // additional length (36-5)
	copy(b[8:16], "RD450X  ") // vendor id (8)
	copy(b[32:36], "1.0 ")    // revision (4)
	return clip(b, alloc)
}

// readTOC returns a minimal single-data-track TOC (formatted, MSF=0).
func (c *Device) readTOC(cdb []byte) []byte {
	// TOC response: 2-byte data length + first/last track, then track descriptors.
	// One data track (track 1) plus the lead-out (0xAA).
	track := func(no byte, lba uint32) []byte {
		d := make([]byte, 8)
		d[1] = 0x14 // ADR/control: data track
		d[2] = no
		binary.BigEndian.PutUint32(d[4:8], lba)
		return d
	}
	body := append(track(1, 0), track(0xAA, c.lastLBA+1)...)
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(body)+2))
	out[2] = 1 // first track
	out[3] = 1 // last track
	copy(out[4:], body)
	return out
}

// getConfiguration returns a minimal GET CONFIGURATION feature header advertising
// a CD-ROM profile.
func (c *Device) getConfiguration() []byte {
	out := make([]byte, 12)
	// Feature header: data length (4) + reserved (2) + current profile (2).
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)-4))
	binary.BigEndian.PutUint16(out[6:8], 0x0008) // current profile: CD-ROM
	return out
}

// readDiscInfo returns a minimal READ DISC INFORMATION response (finalized disc).
func (c *Device) readDiscInfo() []byte {
	out := make([]byte, 34)
	binary.BigEndian.PutUint16(out[0:2], uint16(len(out)-2))
	out[2] = 0x0E // disc status: finalized, last session complete
	out[3] = 1    // first track
	out[4] = 1    // sessions (LSB)
	out[5] = 1    // first track in last session
	out[6] = 1    // last track in last session
	return out
}

// --- small SCSI helpers ------------------------------------------------------

// cdb returns the SCSI CDB embedded in the IUSB payload (starting at opcodeOffset).
func (p *Packet) cdb() []byte {
	if len(p.Payload) <= opcodeOffset {
		return nil
	}
	return p.Payload[opcodeOffset:]
}

// allocLen reads a big-endian allocation length of 1 or 2 bytes from the CDB.
func allocLen(cdb []byte, off, size int) int {
	if size == 2 {
		if len(cdb) >= off+2 {
			return int(binary.BigEndian.Uint16(cdb[off : off+2]))
		}
		return 0
	}
	if len(cdb) > off {
		return int(cdb[off])
	}
	return 0
}

// senseFixed builds an 18-byte fixed-format REQUEST SENSE response.
func senseFixed(key, asc, ascq byte) []byte {
	b := make([]byte, 18)
	b[0] = 0x70 // current error, fixed format
	b[2] = key & 0x0F
	b[7] = 10 // additional sense length
	b[12] = asc
	b[13] = ascq
	return b
}

// getEventStatusNoChange answers GET EVENT STATUS NOTIFICATION with "no change".
func getEventStatusNoChange(cdb []byte) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint16(out[0:2], 2)
	out[2] = 0x00 // NEA=0, event class 0 (no requested classes)
	if len(cdb) > 4 {
		out[3] = cdb[4] // supported event classes echo
	}
	return out
}

// wpBit is the MODE SENSE device-specific write-protect bit, set only when the
// device is read-only — otherwise the host mounts the medium read-only.
func (c *Device) wpBit() byte {
	if c.w == nil {
		return 0x80
	}
	return 0
}

func (c *Device) modeSense6() []byte {
	// Mode parameter header (4 bytes): mode data length, medium type, device-
	// specific params (WP), block descriptor length.
	b := make([]byte, 4)
	b[0] = 3 // mode data length (excludes this byte)
	b[2] = c.wpBit()
	return b
}

func (c *Device) modeSense10() []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:2], 6) // mode data length
	b[3] = c.wpBit()
	return b
}

// clip trims b to at most n bytes (n<=0 means no allocation limit was given).
func clip(b []byte, n int) []byte {
	if n > 0 && n < len(b) {
		return b[:n]
	}
	return b
}
