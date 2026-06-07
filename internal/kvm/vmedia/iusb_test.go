package vmedia

import (
	"encoding/binary"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	in := Header{
		Major:          iusbMajor,
		Minor:          iusbMinor,
		DataPacketLen:  0x12345678,
		DeviceType:     DeviceCDROM,
		Protocol:       1,
		Direction:      128,
		Instance:       3,
		SequenceNumber: 0xCAFEBABE,
	}
	buf := make([]byte, HeaderLen)
	in.marshal(buf)

	got, err := parseHeader(buf)
	if err != nil {
		t.Fatalf("parseHeader: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
}

// The checksum byte must make the unsigned sum of all 32 header bytes ≡ 0 (mod
// 256), which is exactly what the BMC validates.
func TestHeaderChecksumSumsToZero(t *testing.T) {
	h := Header{Major: 1, DataPacketLen: authPayloadLen, DeviceType: DeviceCDROM, Protocol: 1, Direction: 128, Instance: 7}
	buf := make([]byte, HeaderLen)
	h.marshal(buf)

	var sum byte
	for _, b := range buf {
		sum += b
	}
	if sum != 0 {
		t.Errorf("header byte sum = %d, want 0", sum)
	}
	if buf[10] != HeaderLen {
		t.Errorf("packetHeaderLen = %d, want %d", buf[10], HeaderLen)
	}
	if string(buf[0:8]) != signature {
		t.Errorf("signature = %q, want %q", buf[0:8], signature)
	}
}

func TestParseHeaderRejectsBadSignature(t *testing.T) {
	buf := make([]byte, HeaderLen)
	copy(buf, "NOTIUSB!")
	if _, err := parseHeader(buf); err == nil {
		t.Fatal("expected error for bad signature, got nil")
	}
	if _, err := parseHeader(buf[:10]); err == nil {
		t.Fatal("expected error for short header, got nil")
	}
}

func TestBuildAuth(t *testing.T) {
	const token = "STOKEN-abc123"
	pkt := buildAuth(DeviceCDROM, 2, token)

	if len(pkt) != HeaderLen+authPayloadLen {
		t.Fatalf("auth packet len = %d, want %d", len(pkt), HeaderLen+authPayloadLen)
	}
	h, err := parseHeader(pkt)
	if err != nil {
		t.Fatalf("parseHeader: %v", err)
	}
	if h.DeviceType != DeviceCDROM {
		t.Errorf("deviceType = %d, want %d", h.DeviceType, DeviceCDROM)
	}
	if h.Instance != 2 {
		t.Errorf("instance = %d, want 2", h.Instance)
	}
	if h.DataPacketLen != authPayloadLen {
		t.Errorf("dataPacketLen = %d, want %d", h.DataPacketLen, authPayloadLen)
	}
	if h.Direction != 128 {
		t.Errorf("direction = %d, want 128", h.Direction)
	}
	// Opcode 0xF2 must land at absolute offset 41 (header 32 + payload 9), the
	// same offset JViewer writes it to.
	if pkt[HeaderLen+opcodeOffset] != OpAuth {
		t.Errorf("opcode byte = 0x%02X at off %d, want 0x%02X", pkt[HeaderLen+opcodeOffset], HeaderLen+opcodeOffset, OpAuth)
	}
	if got := string(pkt[HeaderLen+authTokenOffset : HeaderLen+authTokenOffset+len(token)]); got != token {
		t.Errorf("token = %q, want %q", got, token)
	}
	// Header checksum still valid after the payload was filled in.
	var sum byte
	for _, b := range pkt[:HeaderLen] {
		sum += b
	}
	if sum != 0 {
		t.Errorf("header checksum invalid after payload write: sum = %d", sum)
	}
}

func TestAckStatus(t *testing.T) {
	mkAck := func(status byte, tail string) *Packet {
		payload := make([]byte, authTokenOffset+len(tail))
		payload[opcodeOffset] = OpRedirectAck
		payload[connStatusOffset] = status
		copy(payload[authTokenOffset:], tail)
		return &Packet{Payload: payload}
	}

	if err := ackStatus(mkAck(connOK, "")); err != nil {
		t.Errorf("status OK: unexpected error %v", err)
	}
	if err := ackStatus(mkAck(connErrInUse5, "")); err == nil {
		t.Error("status 5: expected error, got nil")
	}
	if err := ackStatus(mkAck(99, "192.168.1.42\x00")); err == nil {
		t.Error("in-use status: expected error, got nil")
	} else if !contains(err.Error(), "192.168.1.42") {
		t.Errorf("in-use error should name the owner IP, got %q", err.Error())
	}

	wrong := &Packet{Payload: func() []byte { p := make([]byte, 32); p[opcodeOffset] = 0x28; return p }()}
	if err := ackStatus(wrong); err == nil {
		t.Error("wrong opcode: expected error, got nil")
	}
}

func TestPacketClassifiers(t *testing.T) {
	eject := make([]byte, 16)
	eject[opcodeOffset] = OpStartStopUnit
	eject[ejectByteOffset] = 2
	if !(&Packet{Payload: eject}).IsEject() {
		t.Error("IsEject = false for START STOP UNIT with loej=2")
	}

	kill := make([]byte, 16)
	kill[opcodeOffset] = OpKillRedir
	if !(&Packet{Payload: kill}).IsKill() {
		t.Error("IsKill = false for kill opcode")
	}

	if (&Packet{Payload: []byte{1, 2, 3}}).Opcode() != 0 {
		t.Error("Opcode on too-short payload should be 0")
	}
}

// Guard against accidental endianness drift in the framing-length field, which
// the receive loop relies on to know how many payload bytes follow the header.
func TestDataPacketLenIsLittleEndian(t *testing.T) {
	h := Header{Major: 1, DataPacketLen: 0x00020000} // 128 KiB max transfer
	buf := make([]byte, HeaderLen)
	h.marshal(buf)
	if got := binary.LittleEndian.Uint32(buf[12:16]); got != 0x00020000 {
		t.Errorf("dataPacketLen LE = 0x%08X, want 0x00020000", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
