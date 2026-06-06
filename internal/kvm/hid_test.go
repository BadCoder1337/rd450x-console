package kvm

import (
	"encoding/binary"
	"testing"
)

func TestUsageFor(t *testing.T) {
	cases := []struct {
		name   string
		keysym uint32
		want   byte
	}{
		{"a", 'a', 0x04},
		{"A", 'A', 0x04},
		{"z", 'z', 0x1d},
		{"1", '1', 0x1e},
		{"0", '0', 0x27},
		{"shifted 1 (!)", '!', 0x1e},
		{"Enter", 0xff0d, 0x28},
		{"Escape", 0xff1b, 0x29},
		{"Backspace", 0xff08, 0x2a},
		{"Tab", 0xff09, 0x2b},
		{"Space", ' ', 0x2c},
		{"Up", 0xff52, 0x52},
		{"Down", 0xff54, 0x51},
		{"Left", 0xff51, 0x50},
		{"Right", 0xff53, 0x4f},
		{"F1", 0xffbe, 0x3a},
		{"F12", 0xffc9, 0x45},
		{"minus", '-', 0x2d},
		{"slash", '/', 0x38},
		{"unmapped", 0x12345, 0x00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := usageFor(c.keysym); got != c.want {
				t.Errorf("usageFor(%#x) = %#x, want %#x", c.keysym, got, c.want)
			}
		})
	}
}

func TestModBitFor(t *testing.T) {
	cases := []struct {
		name   string
		keysym uint32
		want   byte
	}{
		{"LeftShift", 0xffe1, modLeftShift},
		{"RightShift", 0xffe2, modRightShift},
		{"LeftCtrl", 0xffe3, modLeftCtrl},
		{"RightCtrl", 0xffe4, modRightCtrl},
		{"LeftAlt", 0xffe9, modLeftAlt},
		{"LeftGUI", 0xffeb, modLeftGUI},
		{"not a modifier", 'a', 0x00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modBitFor(c.keysym); got != c.want {
				t.Errorf("modBitFor(%#x) = %#x, want %#x", c.keysym, got, c.want)
			}
		})
	}
}

// TestModifierBitmask drives the sink through a Ctrl+Alt+Del-style sequence and
// checks byte0 of the resulting USB reports.
func TestModifierBitmask(t *testing.T) {
	s := &HIDSink{pressed: make([]byte, 0, 6)}

	// Press Left Ctrl
	s.KeyEventBuild(0xffe3, true)
	if s.mods != modLeftCtrl {
		t.Fatalf("after Ctrl down: mods=%#x want %#x", s.mods, modLeftCtrl)
	}
	// Press Left Alt
	s.KeyEventBuild(0xffe9, true)
	if s.mods != modLeftCtrl|modLeftAlt {
		t.Fatalf("after Alt down: mods=%#x want %#x", s.mods, modLeftCtrl|modLeftAlt)
	}
	// Press Delete (0xffff → usage 0x4c)
	rep := s.KeyEventBuild(0xffff, true)
	usb := usbFromReport(rep)
	if usb[0] != modLeftCtrl|modLeftAlt {
		t.Errorf("report modifier byte = %#x, want %#x", usb[0], modLeftCtrl|modLeftAlt)
	}
	if usb[2] != 0x4c {
		t.Errorf("report key0 = %#x, want 0x4c (Delete)", usb[2])
	}
	// Release Ctrl
	s.KeyEventBuild(0xffe3, false)
	if s.mods != modLeftAlt {
		t.Fatalf("after Ctrl up: mods=%#x want %#x", s.mods, modLeftAlt)
	}
}

// KeyEventBuild applies the key event to sink state and returns the built report
// without touching the network. It mirrors KeyEvent minus the send.
func (s *HIDSink) KeyEventBuild(keysym uint32, down bool) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bit := modBitFor(keysym); bit != 0 {
		if down {
			s.mods |= bit
		} else {
			s.mods &^= bit
		}
	} else if usage := usageFor(keysym); usage != 0 {
		if down {
			s.addKey(usage)
		} else {
			s.removeKey(usage)
		}
	} else {
		return nil
	}
	return s.buildKeyboardReport()
}

func usbFromReport(rep []byte) []byte { return rep[41:49] }

func TestScaleAbs(t *testing.T) {
	cases := []struct {
		name         string
		x, y, w, h   int
		wantX, wantY int16
	}{
		{"origin", 0, 0, 1024, 768, 0, 0},
		{"full", 1024, 768, 1024, 768, 32767, 32767},
		{"center", 512, 384, 1024, 768, 16384, 16384},
		{"clamp-over", 2000, 2000, 1024, 768, 32767, 32767},
		{"clamp-under", -5, -5, 1024, 768, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gx, gy := scaleAbs(c.x, c.y, c.w, c.h)
			if gx != c.wantX || gy != c.wantY {
				t.Errorf("scaleAbs(%d,%d,%d,%d) = (%d,%d), want (%d,%d)",
					c.x, c.y, c.w, c.h, gx, gy, c.wantX, c.wantY)
			}
		})
	}
}

func TestKeyboardReportLayout(t *testing.T) {
	usb := [8]byte{modLeftShift, 0, 0x04, 0, 0, 0, 0, 0} // Shift+a
	rep := keyboardReport(usb)
	if len(rep) != HeaderSize+41 {
		t.Fatalf("report len = %d, want %d", len(rep), HeaderSize+41)
	}
	// IVTP header
	if got := binary.LittleEndian.Uint16(rep[0:2]); got != opHIDPkt {
		t.Errorf("ivtp type = %d, want %d", got, opHIDPkt)
	}
	if got := binary.LittleEndian.Uint32(rep[2:6]); got != 41 {
		t.Errorf("ivtp size = %d, want 41", got)
	}
	// signature
	if string(rep[8:16]) != "IUSB    " {
		t.Errorf("signature = %q, want %q", rep[8:16], "IUSB    ")
	}
	if rep[16] != 1 || rep[18] != 32 {
		t.Errorf("iusb hdr fields wrong: major=%d size=%d", rep[16], rep[18])
	}
	if rep[25] != iusbDeviceKeybd || rep[26] != iusbProtoKeybd {
		t.Errorf("device/proto = %d/%d, want %d/%d", rep[25], rep[26], iusbDeviceKeybd, iusbProtoKeybd)
	}
	if rep[27] != iusbFromRemote {
		t.Errorf("direction = %#x, want %#x", rep[27], iusbFromRemote)
	}
	if rep[40] != 8 {
		t.Errorf("data length = %d, want 8", rep[40])
	}
	// USB report region
	if rep[41] != modLeftShift || rep[43] != 0x04 {
		t.Errorf("usb report = %v, want mod=%#x key0=0x04", rep[41:49], modLeftShift)
	}
	// Checksum: rep[19] = -(sum(rep[8:40]) & 0xFF) with rep[19] itself = 0
	// during the sum (the builder fills it last). Verify by reconstructing the
	// sum over the header excluding the checksum byte.
	sumNoCk := 0
	for i := 8; i < 40; i++ {
		if i == 19 {
			continue
		}
		sumNoCk += int(rep[i])
	}
	if rep[19] != byte(-(sumNoCk & 0xFF)) {
		t.Errorf("checksum byte = %#x, want %#x", rep[19], byte(-(sumNoCk & 0xFF)))
	}
}

func TestMouseAbsReportLayout(t *testing.T) {
	rep := mouseAbsReport(0x01, 16384, 16384, 0) // left button, center
	if len(rep) != HeaderSize+39 {
		t.Fatalf("report len = %d, want %d", len(rep), HeaderSize+39)
	}
	if got := binary.LittleEndian.Uint32(rep[2:6]); got != 39 {
		t.Errorf("ivtp size = %d, want 39", got)
	}
	if rep[25] != iusbDeviceMouse || rep[26] != iusbProtoMouse {
		t.Errorf("device/proto = %d/%d, want %d/%d", rep[25], rep[26], iusbDeviceMouse, iusbProtoMouse)
	}
	if rep[29] != 1 {
		t.Errorf("ifnum = %d, want 1 (mouse)", rep[29])
	}
	if rep[40] != 6 {
		t.Errorf("data length = %d, want 6", rep[40])
	}
	if rep[41] != 0x01 {
		t.Errorf("button = %#x, want 0x01", rep[41])
	}
	if x := int16(binary.LittleEndian.Uint16(rep[42:44])); x != 16384 {
		t.Errorf("abs x = %d, want 16384", x)
	}
	if y := int16(binary.LittleEndian.Uint16(rep[44:46])); y != 16384 {
		t.Errorf("abs y = %d, want 16384", y)
	}
}

func TestMouseButtonMapping(t *testing.T) {
	// Verify RFB→USB button bit remapping (left/right/middle swap of bit1/bit2).
	cases := []struct {
		name    string
		rfbMask uint8
		wantBtn byte
	}{
		{"none", 0x00, 0x00},
		{"left", 0x01, 0x01},
		{"middle", 0x02, 0x04},
		{"right", 0x04, 0x02},
		{"left+right", 0x05, 0x03},
		{"wheel bits ignored", 0x18, 0x00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rfbButtonsToUSB(c.rfbMask); got != c.wantBtn {
				t.Errorf("rfb mask %#x → usb btn %#x, want %#x", c.rfbMask, got, c.wantBtn)
			}
		})
	}
}

func TestWheel(t *testing.T) {
	if rfbWheel(0x08) != 1 {
		t.Errorf("wheel up should be +1")
	}
	if rfbWheel(0x10) != 0xff {
		t.Errorf("wheel down should be -1 (0xff)")
	}
	if rfbWheel(0x01) != 0 {
		t.Errorf("no wheel bit should be 0")
	}
}
