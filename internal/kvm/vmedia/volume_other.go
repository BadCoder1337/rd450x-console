//go:build !windows

package vmedia

import "fmt"

// Volume passthrough is implemented only on Windows (volume_windows.go). The stub
// keeps the package building on other platforms.
type Volume struct{}

// OpenVolume is unsupported off Windows.
func OpenVolume(letter string, writable bool) (*Volume, error) {
	return nil, fmt.Errorf("vmedia: raw volume passthrough is only supported on Windows")
}

// OpenPhysicalDrive is unsupported off Windows.
func OpenPhysicalDrive(disk int, writable bool) (*Volume, error) {
	return nil, fmt.Errorf("vmedia: physical-disk passthrough is only supported on Windows")
}

// DriveLetterToDisk is unsupported off Windows.
func DriveLetterToDisk(letter string) (int, error) {
	return 0, fmt.Errorf("vmedia: drive-letter resolution is only supported on Windows")
}

func (v *Volume) ReadAt(p []byte, off int64) (int, error)  { return 0, fmt.Errorf("unsupported") }
func (v *Volume) WriteAt(p []byte, off int64) (int, error) { return 0, fmt.Errorf("unsupported") }
func (v *Volume) Size() int64                              { return 0 }
func (v *Volume) Close() error                             { return nil }
