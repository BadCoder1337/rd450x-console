//go:build !windows

package sol

// enableVTOutput is a no-op off Windows: a real Unix terminal already interprets
// VT/ANSI output, and raw mode is handled by term.MakeRaw.
func enableVTOutput() (func(), error) {
	return func() {}, nil
}
