// mkimg_go builds tiny FAT filesystem images from scratch (no mkfs/external
// tooling), as test media for virtual-media floppy and HD/USB redirection — the
// Direct-Access counterpart to mkiso_go's bootable CD.
//
// It writes:
//   - bin/test-fd.img : a 1.44 MB FAT12 floppy image
//   - bin/test-hd.img : a ~16 MB FAT16 HD/USB image (superfloppy: filesystem on
//     the whole device, no partition table — mounts directly, like a USB stick
//     formatted whole-disk)
//
// Each carries a volume label and a README.TXT so the host can confirm it read
// the right bytes (blkid label + mount + cat). Like mkiso_go, the byte layout is
// deterministic, so these double as known-offset fixtures for the data plane.
//
// Usage:  go run ./scripts/mkimg_go [-fd bin/test-fd.img] [-hd bin/test-hd.img]
//
// Reference: Microsoft FAT specification (FAT12/FAT16 BPB, EBPB, directory entry).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
)

const secSize = 512

func main() {
	fdPath := flag.String("fd", "bin/test-fd.img", "output floppy (FAT12) image")
	hdPath := flag.String("hd", "bin/test-hd.img", "output HD/USB (FAT16) image")
	flag.Parse()

	fd := buildFAT(fatParams{
		totalSectors: 2880, // 1.44 MB
		secPerClus:   1,
		rootEntCnt:   224,
		media:        0xF0,
		fat12:        true,
		secPerTrack:  18,
		numHeads:     2,
		driveNum:     0x00,
		label:        "RD450X_FD",
		volID:        0x52443446,
		readme: "RD450X remote console - virtual FLOPPY test image (FAT12).\r\n" +
			"If this file reads back correctly, floppy redirection works.\r\n",
	})
	hd := buildFAT(fatParams{
		totalSectors: 32768, // 16 MB
		secPerClus:   4,
		rootEntCnt:   512,
		media:        0xF8,
		fat12:        false,
		secPerTrack:  32,
		numHeads:     8,
		driveNum:     0x80,
		label:        "RD450X_HD",
		volID:        0x52443448,
		readme: "RD450X remote console - virtual HD/USB test image (FAT16).\r\n" +
			"If this file reads back correctly, HD/USB redirection works.\r\n",
	})

	write(*fdPath, fd)
	write(*hdPath, hd)
}

func write(path string, b []byte) {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes, %d sectors)\n", path, len(b), len(b)/secSize)
}

type fatParams struct {
	totalSectors          int
	secPerClus            int
	rootEntCnt            int
	media                 byte
	fat12                 bool
	secPerTrack, numHeads int
	driveNum              byte
	label                 string
	volID                 uint32
	readme                string
}

func buildFAT(p fatParams) []byte {
	const reserved = 1
	const numFATs = 2
	rootDirSectors := (p.rootEntCnt*32 + secSize - 1) / secSize

	// Solve for the FAT size: clusters depend on it and it depends on clusters.
	// Iterate to a fixed point (the standard mkfs approach).
	fatSize := 1
	for {
		dataSec := p.totalSectors - (reserved + numFATs*fatSize + rootDirSectors)
		clusters := dataSec / p.secPerClus
		var fatBytes int
		if p.fat12 {
			fatBytes = (clusters + 2) * 3 / 2
		} else {
			fatBytes = (clusters + 2) * 2
		}
		need := (fatBytes + secSize - 1) / secSize
		if need <= fatSize {
			break
		}
		fatSize = need
	}

	img := make([]byte, p.totalSectors*secSize)
	writeBPB(img[:secSize], p, reserved, numFATs, fatSize)

	// --- FAT region: entry 0 = media descriptor, entry 1 = EOC, then the file's
	// single-cluster chain (cluster 2 → EOC). ---
	fatOff := reserved * secSize
	dataStartSec := reserved + numFATs*fatSize + rootDirSectors
	clusterCount := (p.totalSectors - dataStartSec) / p.secPerClus
	fat := make([]uint32, clusterCount+2)
	if p.fat12 {
		fat[0] = 0xF00 | uint32(p.media)
		fat[1] = 0xFFF
		fat[2] = 0xFFF // README occupies one cluster, end-of-chain
	} else {
		fat[0] = 0xFF00 | uint32(p.media)
		fat[1] = 0xFFFF
		fat[2] = 0xFFFF
	}
	fatBytes := packFAT(fat, p.fat12)
	for i := 0; i < numFATs; i++ {
		copy(img[fatOff+i*fatSize*secSize:], fatBytes)
	}

	// --- Root directory: volume-label entry + README.TXT entry. ---
	rootOff := (reserved + numFATs*fatSize) * secSize
	writeDirEnt(img[rootOff:rootOff+32], label11(p.label), 0x08, 0, 0) // volume label
	readme := []byte(p.readme)
	writeDirEnt(img[rootOff+32:rootOff+64], fatName("README", "TXT"), 0x20, 2, uint32(len(readme)))

	// --- File data at cluster 2. ---
	copy(img[dataStartSec*secSize:], readme)

	return img
}

func writeBPB(s []byte, p fatParams, reserved, numFATs, fatSize int) {
	s[0], s[1], s[2] = 0xEB, 0x3C, 0x90 // jmp + nop
	copy(s[3:11], "MKIMGGO ")           // OEM name
	binary.LittleEndian.PutUint16(s[11:13], secSize)
	s[13] = byte(p.secPerClus)
	binary.LittleEndian.PutUint16(s[14:16], uint16(reserved))
	s[16] = byte(numFATs)
	binary.LittleEndian.PutUint16(s[17:19], uint16(p.rootEntCnt))
	if p.totalSectors < 0x10000 {
		binary.LittleEndian.PutUint16(s[19:21], uint16(p.totalSectors))
	}
	s[21] = p.media
	binary.LittleEndian.PutUint16(s[22:24], uint16(fatSize))
	binary.LittleEndian.PutUint16(s[24:26], uint16(p.secPerTrack))
	binary.LittleEndian.PutUint16(s[26:28], uint16(p.numHeads))
	if p.totalSectors >= 0x10000 {
		binary.LittleEndian.PutUint32(s[32:36], uint32(p.totalSectors))
	}
	// Extended BPB (FAT12/16).
	s[36] = p.driveNum
	s[38] = 0x29 // extended boot signature
	binary.LittleEndian.PutUint32(s[39:43], p.volID)
	copy(s[43:54], label11(p.label))
	if p.fat12 {
		copy(s[54:62], "FAT12   ")
	} else {
		copy(s[54:62], "FAT16   ")
	}
	s[510], s[511] = 0x55, 0xAA // boot signature
}

// packFAT serializes FAT entries to bytes (12- or 16-bit little-endian packing).
func packFAT(fat []uint32, fat12 bool) []byte {
	if !fat12 {
		b := make([]byte, len(fat)*2)
		for i, e := range fat {
			binary.LittleEndian.PutUint16(b[i*2:], uint16(e))
		}
		return b
	}
	b := make([]byte, (len(fat)*3+1)/2)
	for i := 0; i+1 < len(fat); i += 2 {
		e0, e1 := fat[i]&0xFFF, fat[i+1]&0xFFF
		j := i * 3 / 2
		b[j] = byte(e0)
		b[j+1] = byte((e0>>8)&0x0F) | byte((e1&0x0F)<<4)
		b[j+2] = byte(e1 >> 4)
	}
	if len(fat)%2 == 1 { // trailing odd entry
		e := fat[len(fat)-1] & 0xFFF
		j := (len(fat) - 1) * 3 / 2
		b[j] = byte(e)
		b[j+1] = byte((e >> 8) & 0x0F)
	}
	return b
}

// label11 formats a volume label as one 11-byte space-padded field (not 8.3-split).
func label11(s string) []byte {
	n := []byte("           ") // 11 spaces
	copy(n, strings.ToUpper(s))
	return n
}

// fatName formats an 8.3 name into the 11-byte space-padded directory field.
func fatName(base, ext string) []byte {
	n := []byte("           ") // 11 spaces
	copy(n[0:8], strings.ToUpper(base))
	copy(n[8:11], strings.ToUpper(ext))
	return n
}

// writeDirEnt writes a 32-byte FAT directory entry.
func writeDirEnt(b, name11 []byte, attr byte, firstCluster uint16, size uint32) {
	copy(b[0:11], name11)
	b[11] = attr
	binary.LittleEndian.PutUint16(b[26:28], firstCluster)
	binary.LittleEndian.PutUint32(b[28:32], size)
}
