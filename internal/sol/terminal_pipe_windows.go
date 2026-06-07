//go:build windows

package sol

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVTOutput turns on VT/ANSI processing for stdout so the BMC's escape
// sequences are interpreted by the console host (Windows Terminal / modern
// conhost via ConPTY) rather than printed literally. term.MakeRaw already sets
// ENABLE_VIRTUAL_TERMINAL_INPUT on stdin, but the output handle is separate and
// must be flipped here. The returned func restores the original output mode.
func enableVTOutput() (func(), error) {
	h := windows.Handle(os.Stdout.Fd())

	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}

	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, err
	}

	return func() { _ = windows.SetConsoleMode(h, mode) }, nil
}
