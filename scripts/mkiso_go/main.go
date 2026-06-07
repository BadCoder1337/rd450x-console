// mkiso_go builds a tiny bootable ISO9660 + El Torito image from scratch, with
// no external tooling (no xorriso/genisoimage — none are installed on Windows).
//
// Purpose: a ground-truth test medium for virtual media. Mount the resulting
// image as a virtual CD/DVD (first via the *original* JViewer to confirm the
// BMC presents it to the host, then via our own data plane once it lands) and
// boot the RD450X from it: the El Torito no-emulation boot sector prints a
// recognizable banner via BIOS INT 10h, so a successful boot is visible on the
// console. The disc also carries a README.TXT so it is identifiable when mounted
// read-only as data.
//
// Because the on-disk layout is fixed and documented here, the image doubles as
// a known-offset fixture for unit-testing the AMI IUSB sector-serving data plane
// (internal/kvm/vmedia): every LBA maps to predictable bytes.
//
// Usage:  go run ./scripts/mkiso_go [-o bin/test.iso] [-label RD450X_TEST]
//
// References: ECMA-119 (ISO9660), "El Torito" Bootable CD-ROM Format Spec 1.0.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
)

const sectorSize = 2048

// Fixed sector layout (2048-byte logical blocks). Keeping it static makes the
// image reproducible and usable as a byte-exact test fixture.
const (
	lbaPVD        = 16 // Primary Volume Descriptor
	lbaBootRecord = 17 // El Torito Boot Record Volume Descriptor
	lbaTerminator = 18 // Volume Descriptor Set Terminator
	lbaPathL      = 19 // Path Table (little-endian)
	lbaPathM      = 20 // Path Table (big-endian)
	lbaRootDir    = 21 // root directory extent
	lbaReadme     = 22 // README.TXT content
	lbaBootCat    = 23 // El Torito boot catalog
	lbaBootImg    = 24 // boot image (the 512-byte boot sector)
	totalSectors  = 25
)

func main() {
	out := flag.String("o", "bin/test.iso", "output ISO path")
	label := flag.String("label", "RD450X_TEST", "ISO9660 volume identifier (A-Z0-9_, max 32)")
	flag.Parse()

	img := build(strings.ToUpper(*label))
	if err := os.WriteFile(*out, img, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes, %d sectors)\n", *out, len(img), len(img)/sectorSize)
	fmt.Printf("  bootable: El Torito no-emulation, boot image at LBA %d\n", lbaBootImg)
	fmt.Printf("  mount as virtual CD/DVD and boot — the console should show the banner\n")
}

func build(label string) []byte {
	img := make([]byte, totalSectors*sectorSize)
	sect := func(lba int) []byte { return img[lba*sectorSize : (lba+1)*sectorSize] }

	readme := []byte("RD450X remote console - virtual media test disc.\r\n" +
		"If you can read this file and the machine boots showing the banner,\r\n" +
		"CD/DVD redirection works end-to-end.\r\n")

	writePVD(sect(lbaPVD), label, len(readme))
	writeBootRecord(sect(lbaBootRecord))
	writeTerminator(sect(lbaTerminator))
	writePathTable(sect(lbaPathL), false)
	writePathTable(sect(lbaPathM), true)
	writeRootDir(sect(lbaRootDir), len(readme))
	copy(sect(lbaReadme), readme)
	writeBootCatalog(sect(lbaBootCat))
	copy(sect(lbaBootImg), bootSector())

	return img
}

// --- both-endian field helpers (ISO9660 stores numbers in both orders) -------

// put733 writes a both-endian u32 (8 bytes: LE then BE) — ISO9660 "7.3.3".
func put733(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

// put723 writes a both-endian u16 (4 bytes: LE then BE) — ISO9660 "7.2.3".
func put723(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

func fill(b []byte, c byte) {
	for i := range b {
		b[i] = c
	}
}

// padStr copies s into b, space-padded to len(b) (ISO9660 a/d-string convention).
func padStr(b []byte, s string) {
	fill(b, ' ')
	copy(b, s)
}

// --- volume descriptors ------------------------------------------------------

func writePVD(s []byte, label string, readmeLen int) {
	s[0] = 1 // volume descriptor type: primary
	copy(s[1:6], "CD001")
	s[6] = 1                // version
	padStr(s[8:40], "")     // system identifier
	padStr(s[40:72], label) // volume identifier
	put733(s[80:88], totalSectors)
	put723(s[120:124], 1) // volume set size
	put723(s[124:128], 1) // volume sequence number
	put723(s[128:132], sectorSize)

	put733(s[132:140], uint32(pathTableSize())) // path table size (bytes)
	binary.LittleEndian.PutUint32(s[140:144], lbaPathL)
	binary.BigEndian.PutUint32(s[148:152], lbaPathM)

	// Root directory record (34 bytes), describing the root extent itself.
	writeDirRecord(s[156:190], lbaRootDir, sectorSize, true, "")

	padStr(s[190:318], "") // volume set id
	padStr(s[318:446], "") // publisher id
	padStr(s[446:574], "") // data preparer id
	padStr(s[574:702], "") // application id
	padStr(s[702:739], "") // copyright file id
	padStr(s[739:776], "") // abstract file id
	padStr(s[776:813], "") // bibliographic file id
	noDate(s[813:830])     // creation
	noDate(s[830:847])     // modification
	noDate(s[847:864])     // expiration
	noDate(s[864:881])     // effective
	s[881] = 1             // file structure version
	_ = readmeLen
}

func writeBootRecord(s []byte) {
	s[0] = 0 // boot record
	copy(s[1:6], "CD001")
	s[6] = 1
	padStr(s[7:39], "EL TORITO SPECIFICATION") // boot system identifier (32 bytes, zero-padded per spec; spaces are tolerated)
	// Per spec bytes 7..38 are zero-padded; zero them then write the id without
	// space padding to stay strictly conformant.
	for i := 7; i < 71; i++ {
		s[i] = 0
	}
	copy(s[7:39], "EL TORITO SPECIFICATION")
	binary.LittleEndian.PutUint32(s[71:75], lbaBootCat) // absolute pointer to boot catalog
}

func writeTerminator(s []byte) {
	s[0] = 255
	copy(s[1:6], "CD001")
	s[6] = 1
}

// --- path table --------------------------------------------------------------

// The path table has a single record (the root). Record = 8 + idLen, padded to
// an even length; root id is one 0x00 byte → 10 bytes.
func pathTableSize() int { return 10 }

func writePathTable(s []byte, bigEndian bool) {
	s[0] = 1 // directory identifier length
	s[1] = 0 // extended attribute record length
	if bigEndian {
		binary.BigEndian.PutUint32(s[2:6], lbaRootDir)
		binary.BigEndian.PutUint16(s[6:8], 1) // parent directory number
	} else {
		binary.LittleEndian.PutUint32(s[2:6], lbaRootDir)
		binary.LittleEndian.PutUint16(s[6:8], 1)
	}
	s[8] = 0 // directory identifier: 0x00 == root
	// s[9] padding stays 0
}

// --- directory records -------------------------------------------------------

func writeRootDir(s []byte, readmeLen int) {
	off := 0
	off += writeDirRecord(s[off:], lbaRootDir, sectorSize, true, "")     // "."  (id 0x00)
	off += writeDirRecord(s[off:], lbaRootDir, sectorSize, true, "\x01") // ".." (id 0x01)
	writeDirRecord(s[off:], lbaReadme, uint32(readmeLen), false, "README.TXT;1")
}

// writeDirRecord writes one ISO9660 directory record and returns its byte length.
// Special ids: "" → the "." entry (single 0x00 byte), "\x01" → the ".." entry.
func writeDirRecord(b []byte, extent uint32, dataLen uint32, isDir bool, name string) int {
	var id []byte
	switch name {
	case "":
		id = []byte{0x00}
	case "\x01":
		id = []byte{0x01}
	default:
		id = []byte(name)
	}
	recLen := 33 + len(id)
	if recLen%2 != 0 {
		recLen++ // records are padded to an even length
	}

	b[0] = byte(recLen)
	b[1] = 0 // extended attribute record length
	put733(b[2:10], extent)
	put733(b[10:18], dataLen)
	writeRecDate(b[18:25])
	if isDir {
		b[25] = 0x02 // file flags: directory
	} else {
		b[25] = 0x00
	}
	b[26] = 0           // file unit size
	b[27] = 0           // interleave gap size
	put723(b[28:32], 1) // volume sequence number
	b[32] = byte(len(id))
	copy(b[33:33+len(id)], id)
	return recLen
}

// --- dates -------------------------------------------------------------------

// noDate writes the ISO9660 "not specified" 17-byte date ("0"*16 + tz 0).
func noDate(b []byte) {
	for i := 0; i < 16; i++ {
		b[i] = '0'
	}
	b[16] = 0
}

// writeRecDate writes the 7-byte directory-record timestamp. All-zero is the
// accepted "no date" value and BIOSes ignore it.
func writeRecDate(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// --- El Torito boot catalog --------------------------------------------------

func writeBootCatalog(s []byte) {
	// Validation entry (32 bytes).
	v := s[0:32]
	v[0] = 1 // header id
	v[1] = 0 // platform id: 80x86
	// v[4:28] id string: zeros
	v[30] = 0x55
	v[31] = 0xAA
	// Checksum (u16 LE at 28): the 16-bit word sum of the whole 32-byte entry
	// must be 0.
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(v[i : i+2])
	}
	binary.LittleEndian.PutUint16(v[28:30], uint16(-int16(sum)))

	// Initial/default entry (32 bytes).
	e := s[32:64]
	e[0] = 0x88                              // boot indicator: bootable
	e[1] = 0                                 // boot media type: no emulation
	binary.LittleEndian.PutUint16(e[2:4], 0) // load segment: 0 → default 0x7C00
	e[4] = 0                                 // system type
	binary.LittleEndian.PutUint16(e[6:8], 4) // sector count: 4 virtual 512B sectors (one CD sector)
	binary.LittleEndian.PutUint32(e[8:12], lbaBootImg)
}

// --- boot sector -------------------------------------------------------------

// bootSector returns a 512-byte real-mode boot sector that prints a banner via
// BIOS teletype (INT 10h, AH=0Eh) and halts. El Torito no-emulation loads it at
// 0x7C00 and jumps there in 16-bit real mode.
//
// Hand-assembled (ORG 0x7C00):
//
//	cli
//	xor ax, ax
//	mov ds, ax
//	mov bx, 0x0007        ; page 0, light-grey
//	mov si, msg           ; 0x7C00 + msgOff
//	.print: lodsb
//	        test al, al
//	        jz .hang
//	        mov ah, 0x0E
//	        int 0x10
//	        jmp .print
//	.hang:  hlt
//	        jmp .hang
//	msg db "...",13,10,0
func bootSector() []byte {
	const msgOff = 0x19 // byte offset of msg within the sector (see layout below)
	code := []byte{
		0xFA,       // cli
		0x31, 0xC0, // xor ax, ax
		0x8E, 0xD8, // mov ds, ax
		0xBB, 0x07, 0x00, // mov bx, 0x0007
		0xBE, byte((0x7C00 + msgOff) & 0xFF), byte((0x7C00 + msgOff) >> 8), // mov si, msg
		0xAC,       // print: lodsb
		0x84, 0xC0, // test al, al
		0x74, 0x06, // jz hang
		0xB4, 0x0E, // mov ah, 0x0E
		0xCD, 0x10, // int 0x10
		0xEB, 0xF5, // jmp print
		0xF4,       // hang: hlt
		0xEB, 0xFD, // jmp hang
	}
	if len(code) != msgOff {
		panic(fmt.Sprintf("boot code length %d != msgOff %d", len(code), msgOff))
	}
	sec := make([]byte, 512)
	copy(sec, code)
	copy(sec[msgOff:], []byte("RD450X vmedia test ISO booted OK\r\n\x00"))
	sec[510] = 0x55 // boot signature (harmless for no-emulation; some BIOSes check)
	sec[511] = 0xAA
	return sec
}
