"""Cross-platform raw terminal I/O for an interactive serial console.

Provides a context manager that puts the local terminal into raw mode (no line
buffering, no local echo) and exposes:

  * ``write(data: bytes)``  -- render server output, including ANSI/VT sequences
  * ``read() -> bytes``     -- non-blocking read of pending keystrokes

On Windows, keystrokes are read via ``msvcrt`` and special keys (arrows, Home,
End, etc.) are translated to the ANSI escape sequences a serial console
expects. ANSI *output* is enabled by turning on virtual-terminal processing.

On POSIX, the tty is switched to raw mode with ``termios`` and stdin is read
through ``select``; arrow keys already arrive as escape sequences.
"""

from __future__ import annotations

import codecs
import os
import sys

IS_WINDOWS = os.name == "nt"

# Give a full-screen TUI (BIOS Setup, etc.) a clean canvas:
#   ?1049h  switch to the alternate screen buffer (so the shell screen and
#           scrollback are preserved and restored on exit),
#   2J/3J   clear the screen and scrollback,
#   H       home the cursor,
#   ?7h     ensure auto-wrap is on (serial TUIs assume a normal terminal).
# Without this, the remote draws with absolute cursor addressing over whatever
# (PowerShell output) was already on screen, leaving stale cells everywhere.
ENTER_FULLSCREEN = b"\x1b[?1049h\x1b[2J\x1b[3J\x1b[H\x1b[?7h"
LEAVE_FULLSCREEN = b"\x1b[?1049l"


class _WindowsTerminal:
    # Console mode flags (wincon.h)
    ENABLE_PROCESSED_INPUT = 0x0001
    ENABLE_LINE_INPUT = 0x0002
    ENABLE_ECHO_INPUT = 0x0004
    ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
    ENABLE_QUICK_EDIT_MODE = 0x0040  # click-drag selects text AND pauses output
    ENABLE_EXTENDED_FLAGS = 0x0080   # required for the QuickEdit bit to apply
    ENABLE_VIRTUAL_TERMINAL_INPUT = 0x0200
    STD_INPUT_HANDLE = -10
    STD_OUTPUT_HANDLE = -11

    # Map the byte following the 0x00 / 0xE0 prefix that msvcrt.getch() emits
    # for special keys -> ANSI escape sequence.
    SPECIAL = {
        b"H": b"\x1b[A",   # Up
        b"P": b"\x1b[B",   # Down
        b"M": b"\x1b[C",   # Right
        b"K": b"\x1b[D",   # Left
        b"G": b"\x1b[H",   # Home
        b"O": b"\x1b[F",   # End
        b"I": b"\x1b[5~",  # Page Up
        b"Q": b"\x1b[6~",  # Page Down
        b"R": b"\x1b[2~",  # Insert
        b"S": b"\x1b[3~",  # Delete
        b";": b"\x1bOP",   # F1
        b"<": b"\x1bOQ",   # F2
        b"=": b"\x1bOR",   # F3
        b">": b"\x1bOS",   # F4
    }

    def __init__(self) -> None:
        import ctypes
        from ctypes import wintypes

        self._k = __import__("msvcrt")
        self._ctypes = ctypes
        self._kernel32 = ctypes.windll.kernel32  # type: ignore[attr-defined]
        self._saved_in = None
        self._saved_out = None

        # Incremental UTF-8 decoder so a multi-byte char (BIOS box-drawing) split
        # across SOL packets is not mangled at a write boundary.
        self._dec = codecs.getincrementaldecoder("utf-8")("replace")
        # Write via WriteConsoleW (ctypes) rather than sys.stdout: ctypes
        # releases the GIL around the call, so a slow console render on the
        # render thread cannot starve the network + input loop. (Plain
        # sys.stdout console writes can hold the GIL and freeze everything.)
        self._write_console = self._kernel32.WriteConsoleW
        self._write_console.argtypes = [
            wintypes.HANDLE,
            wintypes.LPCWSTR,
            wintypes.DWORD,
            ctypes.POINTER(wintypes.DWORD),
            wintypes.LPVOID,
        ]
        self._write_console.restype = wintypes.BOOL
        self._nwritten = wintypes.DWORD()

    def _get_mode(self, handle):
        mode = self._ctypes.c_uint32()
        self._kernel32.GetConsoleMode(handle, self._ctypes.byref(mode))
        return mode.value

    def __enter__(self):
        k = self._kernel32
        self._hin = k.GetStdHandle(self.STD_INPUT_HANDLE)
        self._hout = k.GetStdHandle(self.STD_OUTPUT_HANDLE)
        self._saved_in = self._get_mode(self._hin)
        self._saved_out = self._get_mode(self._hout)

        # Output: enable ANSI/VT so the server's escape sequences render.
        out_mode = self._saved_out | self.ENABLE_VIRTUAL_TERMINAL_PROCESSING
        k.SetConsoleMode(self._hout, out_mode)

        # Input: raw. msvcrt.getch() bypasses line discipline anyway, but we
        # also clear the cooked-mode flags so nothing is echoed or pre-chewed,
        # and disable QuickEdit so a stray click in the window cannot freeze
        # output mid-session. ENABLE_EXTENDED_FLAGS must be set for the
        # QuickEdit change to take effect.
        in_mode = self._saved_in & ~(
            self.ENABLE_LINE_INPUT
            | self.ENABLE_ECHO_INPUT
            | self.ENABLE_PROCESSED_INPUT
            | self.ENABLE_QUICK_EDIT_MODE
        )
        in_mode |= self.ENABLE_EXTENDED_FLAGS
        k.SetConsoleMode(self._hin, in_mode)
        self.write(ENTER_FULLSCREEN)
        return self

    def __exit__(self, *exc):
        try:
            self.write(LEAVE_FULLSCREEN)
        except Exception:
            pass
        if self._saved_in is not None:
            self._kernel32.SetConsoleMode(self._hin, self._saved_in)
        if self._saved_out is not None:
            self._kernel32.SetConsoleMode(self._hout, self._saved_out)

    def read(self) -> bytes:
        out = bytearray()
        k = self._k
        while k.kbhit():
            ch = k.getch()
            if ch in (b"\x00", b"\xe0"):
                # Special key: the next getch() is the scan code.
                code = k.getch()
                out += self.SPECIAL.get(code, b"")
            else:
                out += ch
        return bytes(out)

    def write(self, data: bytes) -> None:
        text = self._dec.decode(data)
        if not text:
            return
        handle = getattr(self, "_hout", None)
        if handle is None:  # before __enter__ / not a console
            sys.stdout.buffer.write(text.encode("utf-8", "replace"))
            sys.stdout.buffer.flush()
            return
        i, n, chunk = 0, len(text), 8000
        while i < n:
            part = text[i:i + chunk]
            ok = self._write_console(
                handle, part, len(part),
                self._ctypes.byref(self._nwritten), None,
            )
            if not ok:  # redirected / not a console -- fall back to stdout
                sys.stdout.buffer.write(part.encode("utf-8", "replace"))
                sys.stdout.buffer.flush()
                i += len(part)
                continue
            i += self._nwritten.value or len(part)


class _PosixTerminal:
    def __init__(self) -> None:
        import select
        import termios
        import tty

        self._termios = termios
        self._tty = tty
        self._select = select
        self._fd = sys.stdin.fileno()
        self._saved = None

    def __enter__(self):
        self._saved = self._termios.tcgetattr(self._fd)
        self._tty.setraw(self._fd)
        self.write(ENTER_FULLSCREEN)
        return self

    def __exit__(self, *exc):
        try:
            self.write(LEAVE_FULLSCREEN)
        except Exception:
            pass
        if self._saved is not None:
            self._termios.tcsetattr(
                self._fd, self._termios.TCSADRAIN, self._saved
            )

    def read(self) -> bytes:
        r, _, _ = self._select.select([self._fd], [], [], 0)
        if not r:
            return b""
        return os.read(self._fd, 4096)

    def write(self, data: bytes) -> None:
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()


def RawTerminal():
    """Return the platform-appropriate raw-terminal context manager."""
    return _WindowsTerminal() if IS_WINDOWS else _PosixTerminal()
