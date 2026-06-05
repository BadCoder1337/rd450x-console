"""Serial-over-LAN console session built on standard IPMI 2.0 RMCP+.

Design (single owner of pyghmi + a decoupled renderer):

  * **main thread** -- owns *all* pyghmi I/O. Each iteration pumps the session
    (``wait_for_rsp`` -> receive + ACK, which a probe showed stays ~40 ms even
    under a 11 KB/87-packet BIOS repaint), drives a non-blocking keystroke
    retransmit, reads local keystrokes, runs the escape state machine, and
    sends. Received server bytes are pushed onto a render queue -- never written
    to the terminal here.
  * **render thread** -- drains the queue and writes to the terminal. Writing a
    big VT burst to a Windows console can block (slow conhost, QuickEdit pause);
    isolating it means the network keeps ACKing (session stays alive) and the
    escape hotkeys keep working even while the screen is catching up.

Sending is also made non-blocking (see ``_async_console_class``) so typing
never stalls the loop either.
"""

from __future__ import annotations

import queue
import sys
import threading
import time

from .config import Config
from .terminal import RawTerminal

# Default escape (attention) key: Ctrl-]  (ASCII GS, 0x1d) -- same key telnet
# uses, so it is familiar and rarely needed by the remote shell.
DEFAULT_ESCAPE = 0x1D

_async_console_cls = None


def _async_console_class():
    """Lazily build a pyghmi Console subclass with a *non-blocking* send.

    The stock ``Console._sendoutput`` busy-waits up to ~12 s for the BMC to ACK
    each chunk, which freezes the loop whenever the BMC is busy. Here we send
    once and return; un-ACKed chunks are retransmitted from the main loop via
    ``tick()`` instead of a blocking wait.
    """
    global _async_console_cls
    if _async_console_cls is not None:
        return _async_console_cls

    from pyghmi.ipmi import console as ipmiconsole

    class _AsyncConsole(ipmiconsole.Console):
        _RESEND_AFTER = 0.6   # seconds before retransmitting an un-ACKed chunk
        _MAX_RESENDS = 8

        def _sendoutput(self, output, sendbreak=False):
            self.myseq = ((self.myseq + 1) & 0xF) or 1
            breakbyte = 0b10000 if sendbreak else 0
            try:
                payload = bytearray((self.myseq, 0, 0, breakbyte)) + output
            except TypeError:  # bytearray hits unicode
                payload = bytearray(
                    (self.myseq, 0, 0, breakbyte)
                ) + output.encode("utf8")
            self.lasttextsize = len(output)
            self.awaitingack = True
            self.lastpayload = payload
            self._last_send = time.monotonic()
            self._resends = 0
            self.send_payload(
                payload, retry=False, needskeepalive=(self.lasttextsize == 0)
            )

        def tick(self):
            """Non-blocking retransmit; call once per main-loop iteration."""
            if not self.awaitingack:
                return
            now = time.monotonic()
            if now - getattr(self, "_last_send", now) < self._RESEND_AFTER:
                return
            if getattr(self, "_resends", 0) >= self._MAX_RESENDS:
                self.awaitingack = False  # give up so new input can flow
                return
            self._resends = getattr(self, "_resends", 0) + 1
            self._last_send = now
            try:
                self.send_payload(self.lastpayload, retry=False)
            except Exception:
                pass

    _async_console_cls = _AsyncConsole
    return _async_console_cls


class _EscapeHandler:
    """Telnet-style escape state machine.

    Normal bytes pass through to the BMC. When the escape byte is seen, the
    *next* byte is treated as a command rather than data.
    """

    def __init__(self, console: "SerialConsole", escape: int) -> None:
        self.console = console
        self.escape = escape
        self.armed = False

    def feed(self, data: bytes) -> bytes:
        """Return the bytes that should actually be sent to the BMC."""
        passthrough = bytearray()
        for byte in data:
            if self.armed:
                self.armed = False
                passthrough += self._command(byte)
            elif byte == self.escape:
                self.armed = True
            else:
                passthrough.append(byte)
        return bytes(passthrough)

    def _command(self, byte: int) -> bytes:
        ch = chr(byte).lower()
        if ch in ("q", "."):
            self.console.request_stop()
            self.console.notice("escape: quit")
            return b""
        if ch == "b":
            self.console.notice("escape: sending serial break")
            self.console.send_break()
            return b""
        if ch == "?":
            self.console.show_help()
            return b""
        if byte == self.escape:
            # Escape pressed twice -> send one literal escape byte.
            return bytes([self.escape])
        self.console.notice(
            f"escape: unknown command {ch!r} (press Ctrl-] ? for help)"
        )
        return b""


class SerialConsole:
    def __init__(
        self, config: Config, escape: int = DEFAULT_ESCAPE, force: bool = False
    ) -> None:
        self.config = config
        self.escape = escape
        self.force = force
        self._term = None
        self._console = None
        self._stop = False
        self._error: str | None = None
        # Server bytes / status lines are queued here and written to the
        # terminal by a dedicated render thread, so a slow/blocking console
        # write never stalls the network + input loop.
        self._renderq: "queue.Queue[bytes]" = queue.Queue()

    # ---- io-handler: called by pyghmi from inside wait_for_rsp (main loop) ----
    def _on_output(self, data) -> None:
        if isinstance(data, dict):
            if "error" in data:
                self._error = str(data["error"])
                self._stop = True
                self.notice(f"[{data['error']}]")
            elif "info" in data:
                self.notice(f"[{data['info']}]")
            else:
                self.notice(f"[{data}]")
            return
        # Raw server bytes -> hand off to the render thread.
        self._renderq.put(bytes(data))

    def notice(self, message: str) -> None:
        # Status lines are framed on their own CRLF line so they do not corrupt
        # the remote screen. Queued like everything else to keep ordering and a
        # single terminal writer.
        if self._term is not None:
            self._renderq.put(b"\r\n*** " + message.encode() + b"\r\n")
        else:
            sys.stderr.write(f"*** {message}\n")

    def request_stop(self) -> None:
        self._stop = True

    def send_break(self) -> None:
        if self._console is not None:
            self._console.send_break()

    def show_help(self) -> None:
        esc = f"Ctrl-{chr(self.escape + 0x40)}"  # 0x1d -> 'Ctrl-]'
        self.notice(
            "escape commands: "
            f"{esc} q = quit | {esc} b = break | "
            f"{esc} {esc} = literal | {esc} ? = help"
        )

    # ---- render thread ----
    def _render_loop(self, term) -> None:
        while not self._stop:
            try:
                chunk = self._renderq.get(timeout=0.1)
            except queue.Empty:
                continue
            buf = bytearray(chunk)
            try:  # coalesce everything else already queued into one write
                while True:
                    buf += self._renderq.get_nowait()
            except queue.Empty:
                pass
            try:
                term.write(bytes(buf))
            except Exception:
                pass

    # ---- main entry point ----
    def run(self) -> int:
        from pyghmi.exceptions import IpmiException
        from pyghmi.ipmi import console as ipmiconsole

        cfg = self.config
        esc_label = f"Ctrl-{chr(self.escape + 0x40)}"
        print(
            f"Connecting to {cfg.host}:{cfg.port} as {cfg.user!r} "
            f"(SOL / IPMI 2.0 RMCP+) ...",
            flush=True,
        )
        try:
            self._console = _async_console_class()(
                bmc=cfg.host,
                userid=cfg.user,
                password=cfg.password,
                port=cfg.port,
                iohandler=self._on_output,
                force=self.force,
            )
        except IpmiException as exc:
            print(f"Failed to open SOL session: {exc}", file=sys.stderr)
            return 1

        if self._error:
            print(f"Could not activate SOL: {self._error}", file=sys.stderr)
            if not self.force and "another client" in self._error.lower():
                print(
                    "Hint: a stale SOL session is held on the BMC. "
                    "Re-run with --force to take it over.",
                    file=sys.stderr,
                )
            return 1

        print(
            f"Connected. Escape key is {esc_label}; "
            f"press {esc_label} ? for commands, {esc_label} q to quit.",
            flush=True,
        )

        handler = _EscapeHandler(self, self.escape)
        rc = 0
        with RawTerminal() as term:
            self._term = term
            render = threading.Thread(
                target=self._render_loop, args=(term,), daemon=True
            )
            render.start()
            try:
                while not self._stop:
                    # 1. Pump the session: receive (-> render queue) and ACK.
                    ipmiconsole.Console.wait_for_rsp(0.02)
                    # 2. Drive non-blocking retransmit of any un-ACKed keystroke.
                    self._console.tick()
                    if self._stop or self._console.broken:
                        break
                    # 3. Forward local keystrokes; escape commands handled first.
                    data = term.read()
                    if data:
                        outbound = handler.feed(data)
                        if outbound and not self._stop:
                            self._console.send_data(outbound)
            except KeyboardInterrupt:
                pass
            except IpmiException as exc:
                self._error = str(exc)
                rc = 1
            finally:
                self._stop = True
                render.join(timeout=0.5)
                self._term = None

        try:
            self._console.close()
        except Exception:
            pass

        if self._error:
            print(f"\nSession ended: {self._error}", file=sys.stderr)
            rc = rc or 1
        else:
            print("\nSession closed.")
        return rc
