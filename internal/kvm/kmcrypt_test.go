package kvm

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncryptKeyboardReport(t *testing.T) {
	crypt, err := newKMCrypt("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{kmCrypt: crypt}
	usb := [8]byte{2, 0, 4} // Shift+A
	plain := keyboardReport(usb)

	got, err := c.encryptHIDReport(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 49 {
		t.Fatalf("encrypted packet length = %d, want 49", len(got))
	}
	if size := binary.LittleEndian.Uint32(got[2:6]); size != 41 {
		t.Errorf("IVTP payload size = %d, want 41", size)
	}
	if status := binary.LittleEndian.Uint16(got[6:8]); status != 0xff {
		t.Errorf("IVTP status = %#x, want 0xff", status)
	}
	if !bytes.Equal(got[8:41], plain[8:41]) {
		t.Error("IVTP-independent IUSB header changed")
	}
	if bytes.Equal(got[41:], usb[:]) {
		t.Error("USB report was not encrypted")
	}
}

func TestEncryptMouseReportPadsToBlowfishBlock(t *testing.T) {
	crypt, err := newKMCrypt("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{kmCrypt: crypt}
	got, err := c.encryptHIDReport(mouseAbsReport(1, 123, 456, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 49 {
		t.Fatalf("encrypted mouse packet length = %d, want 49", len(got))
	}
	if got[40] != 6 {
		t.Errorf("clear USB data length = %d, want 6", got[40])
	}
}
