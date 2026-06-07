//go:build windows

package vmedia

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// Windows IOCTL/FSCTL codes for raw volume / disk access.
const (
	ioctlDiskGetLengthInfo      = 0x0007405C // IOCTL_DISK_GET_LENGTH_INFO
	fsctlLockVolume             = 0x00090018 // FSCTL_LOCK_VOLUME
	fsctlUnlockVolume           = 0x0009001C // FSCTL_UNLOCK_VOLUME
	fsctlDismountVolume         = 0x00090020 // FSCTL_DISMOUNT_VOLUME
	ioctlStorageGetDeviceNumber = 0x002D1080 // IOCTL_STORAGE_GET_DEVICE_NUMBER
	ioctlDiskSetDiskAttributes  = 0x0007C0F4 // IOCTL_DISK_SET_DISK_ATTRIBUTES
	ioctlDiskUpdateProperties   = 0x00070140 // IOCTL_DISK_UPDATE_PROPERTIES
	diskAttributeOffline        = 0x01       // DISK_ATTRIBUTE_OFFLINE
)

// Volume is a raw Windows volume opened for whole-disk passthrough (e.g. a USB
// stick mounted as "Y:"). It satisfies Reader (and ReadWriter when writable), so
// a real device can be redirected to the host exactly like a file image.
//
// Raw volume access requires the process to run **elevated (Administrator)** —
// the same reason JViewer demands admin for device redirection. All I/O is at the
// 512-byte sector granularity the BMC's Direct-Access commands use, so positional
// ReadFile/WriteFile with an offset is always sector-aligned.
//
// For writable passthrough the volume is locked and dismounted so Windows stops
// touching the filesystem while the remote host owns it (preventing dual-access
// corruption); Windows remounts it on Close.
type Volume struct {
	h        windows.Handle
	size     int64
	writable bool
	physical bool // whole physical disk (\\.\PhysicalDriveN) vs a single volume
	mu       sync.Mutex
}

// OpenVolume opens a drive letter (e.g. "Y:" or "Y") as a raw volume. writable
// also locks + dismounts it.
func OpenVolume(letter string, writable bool) (*Volume, error) {
	l := strings.TrimRight(strings.TrimSpace(letter), `:\`)
	if len(l) != 1 {
		return nil, fmt.Errorf("vmedia: %q is not a drive letter (want e.g. Y:)", letter)
	}
	path := `\\.\` + strings.ToUpper(l) + `:`

	access := uint32(windows.GENERIC_READ)
	if writable {
		access |= windows.GENERIC_WRITE
	}
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path), access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("vmedia: open %s: %w (run elevated/as Administrator?)", path, err)
	}

	v := &Volume{h: h, writable: writable}
	size, err := volumeLength(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("vmedia: get length of %s: %w", path, err)
	}
	v.size = size

	if writable {
		// Take the volume offline so Windows' cache can't fight the remote host.
		if err := deviceIoctl(h, fsctlLockVolume); err != nil {
			windows.CloseHandle(h)
			return nil, fmt.Errorf("vmedia: lock %s failed: %w (close anything using the drive)", path, err)
		}
		if err := deviceIoctl(h, fsctlDismountVolume); err != nil {
			deviceIoctl(h, fsctlUnlockVolume)
			windows.CloseHandle(h)
			return nil, fmt.Errorf("vmedia: dismount %s failed: %w", path, err)
		}
	}
	return v, nil
}

// OpenPhysicalDrive opens a whole physical disk (\\.\PhysicalDriveN) for
// end-to-end device passthrough — the host then sees the entire USB device
// including its partition table, not just one volume. This is the granularity a
// future WebUSB byte-source would expose. writable takes the disk **offline**
// (which dismounts all its volumes) so Windows can't race the remote host; Close
// brings it back online.
func OpenPhysicalDrive(disk int, writable bool) (*Volume, error) {
	path := fmt.Sprintf(`\\.\PhysicalDrive%d`, disk)
	access := uint32(windows.GENERIC_READ)
	if writable {
		access |= windows.GENERIC_WRITE
	}
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path), access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("vmedia: open %s: %w (run elevated/as Administrator?)", path, err)
	}

	v := &Volume{h: h, writable: writable, physical: true}
	size, err := volumeLength(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("vmedia: get length of %s: %w", path, err)
	}
	v.size = size

	if writable {
		if err := setDiskOffline(h, true); err != nil {
			windows.CloseHandle(h)
			return nil, fmt.Errorf("vmedia: take disk %d offline failed: %w (close anything using its volumes)", disk, err)
		}
		deviceIoctl(h, ioctlDiskUpdateProperties)
	}
	return v, nil
}

// DriveLetterToDisk resolves a drive letter (e.g. "Y:") to the physical disk
// number that backs it. Needs no elevation (a query-only handle).
func DriveLetterToDisk(letter string) (int, error) {
	l := strings.TrimRight(strings.TrimSpace(letter), `:\`)
	if len(l) != 1 {
		return 0, fmt.Errorf("vmedia: %q is not a drive letter (want e.g. Y:)", letter)
	}
	path := `\\.\` + strings.ToUpper(l) + `:`
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path), 0, // query only — no access rights needed
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("vmedia: open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	// STORAGE_DEVICE_NUMBER { DeviceType DWORD; DeviceNumber DWORD; PartitionNumber DWORD }
	var out [12]byte
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlStorageGetDeviceNumber, nil, 0,
		&out[0], uint32(len(out)), &ret, nil); err != nil {
		return 0, fmt.Errorf("vmedia: get device number of %s: %w", path, err)
	}
	return int(binary.LittleEndian.Uint32(out[4:8])), nil
}

// setDiskOffline toggles the DISK_ATTRIBUTE_OFFLINE attribute on a physical-disk
// handle (non-persistent). Offline dismounts the disk's volumes and grants
// exclusive raw access.
func setDiskOffline(h windows.Handle, offline bool) error {
	// SET_DISK_ATTRIBUTES (40 bytes): Version@0, Persist@4, Attributes@8,
	// AttributesMask@16, Reserved2@24.
	var buf [40]byte
	binary.LittleEndian.PutUint32(buf[0:4], 40)
	var attr uint64
	if offline {
		attr = diskAttributeOffline
	}
	binary.LittleEndian.PutUint64(buf[8:16], attr)
	binary.LittleEndian.PutUint64(buf[16:24], diskAttributeOffline) // mask
	var ret uint32
	return windows.DeviceIoControl(h, ioctlDiskSetDiskAttributes,
		&buf[0], uint32(len(buf)), nil, 0, &ret, nil)
}

func (v *Volume) Size() int64 { return v.size }

func (v *Volume) ReadAt(p []byte, off int64) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ov := &windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	var done uint32
	err := windows.ReadFile(v.h, p, &done, ov)
	return int(done), err
}

func (v *Volume) WriteAt(p []byte, off int64) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ov := &windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	var done uint32
	err := windows.WriteFile(v.h, p, &done, ov)
	return int(done), err
}

// Close flushes and releases the device: a volume is unlocked (remounted), a
// physical disk is brought back online.
func (v *Volume) Close() error {
	if v.writable {
		windows.FlushFileBuffers(v.h)
		if v.physical {
			setDiskOffline(v.h, false) // back online
			deviceIoctl(v.h, ioctlDiskUpdateProperties)
		} else {
			deviceIoctl(v.h, fsctlUnlockVolume)
		}
	}
	return windows.CloseHandle(v.h)
}

func volumeLength(h windows.Handle) (int64, error) {
	// IOCTL_DISK_GET_LENGTH_INFO returns GET_LENGTH_INFORMATION { LARGE_INTEGER Length }.
	var out [8]byte
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlDiskGetLengthInfo, nil, 0,
		&out[0], uint32(len(out)), &ret, nil); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(out[:])), nil
}

func deviceIoctl(h windows.Handle, code uint32) error {
	var ret uint32
	return windows.DeviceIoControl(h, code, nil, 0, nil, 0, &ret, nil)
}
