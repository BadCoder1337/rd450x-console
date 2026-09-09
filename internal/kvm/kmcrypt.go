package kvm

import (
	"encoding/binary"
	"fmt"
	"log"
	"sync"

	"golang.org/x/crypto/blowfish"
)

// kmCrypt reproduces JViewer's KMCrypt: Blowfish in ECB mode with zero-padding
// to an 8-byte boundary. Java's Cipher.doFinal resets after every report; ECB
// has no cross-report state, so a single immutable block cipher is sufficient.
type kmCrypt struct {
	mu    sync.Mutex
	block *blowfish.Cipher
}

func newKMCrypt(token string) (*kmCrypt, error) {
	key := []byte(token)
	if len(key) < 16 {
		padded := make([]byte, 16)
		copy(padded, key)
		key = padded
	}
	block, err := blowfish.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize Blowfish: %w", err)
	}
	return &kmCrypt{block: block}, nil
}

func (k *kmCrypt) encrypt(plain []byte) []byte {
	n := (len(plain) + blowfish.BlockSize - 1) / blowfish.BlockSize * blowfish.BlockSize
	out := make([]byte, n)
	copy(out, plain)
	k.mu.Lock()
	defer k.mu.Unlock()
	for off := 0; off < len(out); off += blowfish.BlockSize {
		k.block.Encrypt(out[off:off+blowfish.BlockSize], out[off:off+blowfish.BlockSize])
	}
	return out
}

// enableKMEncryption reacts to ENCRYPTION_STATUS (14) / INITIAL_ENCRYPTION_STATUS
// (15). Receipt of either is treated as "encrypt keyboard/mouse reports from now
// on", following the decompiled JViewer; the packet's status word and body are
// logged (never the key) because no live BMC has yet been seen sending these,
// and a firmware that encodes an on/off flag here would need this to be refined.
func (c *Client) enableKMEncryption(h header, body []byte) {
	if c.kmCrypt == nil {
		log.Printf("kvm: BMC sent encryption status (type %d, status %d, body %x) but Blowfish is unavailable; input will be rejected", h.Type, h.Status, body)
		return
	}
	if c.kmEncryption.CompareAndSwap(false, true) {
		// Do not log key/token material.
		log.Printf("kvm: BMC requires encrypted keyboard/mouse reports (type %d, status %d, body %x)", h.Type, h.Status, body)
	}
}

// encryptHIDReport converts the normal IVTP/IUSB HID packet into JViewer's
// encrypted form. The fixed 41-byte prefix is unchanged except for IVTP size
// and status; only the raw USB report at offset 41 is encrypted and padded.
func (c *Client) encryptHIDReport(report []byte) ([]byte, error) {
	if c.kmCrypt == nil {
		return nil, fmt.Errorf("kvm: BMC requires keyboard/mouse encryption, but Blowfish is unavailable")
	}
	if len(report) < 42 {
		return nil, fmt.Errorf("kvm: short HID report: %d bytes", len(report))
	}
	ciphertext := c.kmCrypt.encrypt(report[41:])
	out := make([]byte, 41+len(ciphertext))
	copy(out, report[:41])
	copy(out[41:], ciphertext)
	binary.LittleEndian.PutUint32(out[2:6], uint32(len(out)-HeaderSize))
	binary.LittleEndian.PutUint16(out[6:8], 0x00ff)
	return out, nil
}

func mouseModeName(mode byte) string {
	switch mode {
	case 1:
		return "relative"
	case 2:
		return "absolute"
	case 3:
		return "other"
	default:
		return "unknown"
	}
}
