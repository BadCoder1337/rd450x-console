package vmedia

import (
	"encoding/binary"
	"testing"
)

// patReader returns deterministic bytes (byte i == i%251) so reads are verifiable
// without a real image.
type patReader struct{ size int64 }

func (p patReader) Size() int64 { return p.size }
func (p patReader) ReadAt(b []byte, off int64) (int, error) {
	for i := range b {
		b[i] = byte((off + int64(i)) % 251)
	}
	return len(b), nil
}

// TestRead10Golden replays the exact READ(10) request captured from the live BMC
// and asserts the response the host accepted: the command envelope echoed, the
// SCSI data appended, and BOTH length fields (envOffDataLen and the decisive
// envOffRespLen) set to the byte count. Guards the wire format against drift —
// this is precisely the contract that, when broken, gave the host DID_ERROR.
func TestRead10Golden(t *testing.T) {
	// Real capture: READ(10), LBA 14, 2 blocks (offset 28672, 4096 bytes).
	req := &Packet{Payload: []byte{
		0x00, 0x10, 0x00, 0x00, 0x61, 0x00, 0x00, 0x00,
		0x01, 0x28, 0x00, 0x00, 0x00, 0x00, 0x0e, 0x00,
		0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
	}}
	if req.Opcode() != scsiRead10 {
		t.Fatalf("opcode = 0x%02X, want READ(10) 0x28", req.Opcode())
	}

	cd := NewCDROM(patReader{size: 1 << 20})
	resp, err := cd.Handle(req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	const wantData = 2 * cdBlockSize // 4096
	if len(resp) != len(req.Payload)+wantData {
		t.Fatalf("resp len = %d, want %d", len(resp), len(req.Payload)+wantData)
	}
	// Envelope echoed verbatim except the length fields we stamp.
	if binary.LittleEndian.Uint32(resp[envOffDataLen:envOffDataLen+4]) != wantData {
		t.Errorf("envOffDataLen = %d, want %d", binary.LittleEndian.Uint32(resp[0:4]), wantData)
	}
	if got := binary.LittleEndian.Uint32(resp[envOffRespLen : envOffRespLen+4]); got != wantData {
		t.Errorf("envOffRespLen = %d, want %d (the field the BMC actually forwards)", got, wantData)
	}
	// Appended data must be the image bytes at LBA 14 (offset 28672).
	data := resp[len(req.Payload):]
	for i := 0; i < 8; i++ {
		want := byte((28672 + i) % 251)
		if data[i] != want {
			t.Fatalf("data[%d] = %d, want %d (wrong read offset)", i, data[i], want)
		}
	}
}

// TestTestUnitReadyEcho verifies the media-present poll: a status-only reply that
// echoes the request envelope unchanged (no data, zero length).
func TestTestUnitReadyEcho(t *testing.T) {
	req := &Packet{Payload: []byte{
		0x00, 0x00, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
	}}
	cd := NewCDROM(patReader{size: 1 << 20})
	resp, err := cd.Handle(req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(resp) != len(req.Payload) {
		t.Fatalf("TUR resp len = %d, want %d (echo, no data)", len(resp), len(req.Payload))
	}
}

func TestReadCapacity(t *testing.T) {
	cd := NewCDROM(patReader{size: 25 * cdBlockSize}) // 25 blocks → last LBA 24
	cap := cd.readCapacity()
	if got := binary.BigEndian.Uint32(cap[0:4]); got != 24 {
		t.Errorf("last LBA = %d, want 24", got)
	}
	if got := binary.BigEndian.Uint32(cap[4:8]); got != cdBlockSize {
		t.Errorf("block size = %d, want %d", got, cdBlockSize)
	}
}

// NewDisk (floppy/HD/USB) must use 512-byte Direct-Access blocks: READ CAPACITY
// reports 512 and a READ(10) addresses 512-byte LBAs (so LBA 3 == byte 1536).
func TestDiskProfile512(t *testing.T) {
	d := NewDisk(patReader{size: 2880 * diskBlockSize}) // 1.44 MB floppy
	cap := d.readCapacity()
	if got := binary.BigEndian.Uint32(cap[4:8]); got != diskBlockSize {
		t.Errorf("disk block size = %d, want %d", got, diskBlockSize)
	}
	if got := binary.BigEndian.Uint32(cap[0:4]); got != 2879 {
		t.Errorf("last LBA = %d, want 2879", got)
	}

	// READ(10): LBA 3, 1 block → offset 1536, 512 bytes. CDB at payload[9]:
	// opcode 0x28, LBA (BE) at [11:15] = 00 00 00 03, block count (BE) at [16:18] = 00 01.
	req := &Packet{Payload: []byte{
		0x00, 0x02, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x28, 0x00, 0x00, 0x00, 0x00, 0x03, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00,
	}}
	resp, err := d.Handle(req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	data := resp[len(req.Payload):]
	if len(data) != diskBlockSize {
		t.Fatalf("read len = %d, want %d", len(data), diskBlockSize)
	}
	if want := byte((3*diskBlockSize + 0) % 251); data[0] != want {
		t.Errorf("data[0] = %d, want %d (wrong 512-byte LBA scaling)", data[0], want)
	}
}
