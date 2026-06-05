"""Serial-over-LAN console session built on standard IPMI 2.0 RMCP+.

Wraps pyghmi's SOL ``Console`` in a single-threaded event loop. All pyghmi
calls happen on one thread (the loop), so we avoid the session library's
thread-safety pitfalls. The loop:

  1. pumps the IPMI session (``wait_for_rsp``) -- this delivers server output
     to our io-handler callback,
  2. reads pending local keystrokes and forwards them to the BMC,
  3. intercepts an escape key (default Ctrl-]) to drive a small command menu
     (quit, send serial break, send a literal escape byte).
"""

from __future__ import annotations

import sys

from .config import Config
from .terminal import RawTerminal

# Default escape (attention) key: Ctrl-]  (ASCII GS, 0x1d) -- same key telnet
# uses, so it is familiar and rarely needed by the remote shell.
DEFAULT_ESCAPE = 0x1D


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
            self.console.notice("escape: quit")
            self.console.request_stop()
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
        # Unknown command: surface it and send nothing.
        self.console.notice(f"escape: unknown command {ch!r} (press Ctrl-] ? for help)")
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

    # ---- io-handler: called by pyghmi from inside wait_for_rsp ----
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
        # Raw server bytes.
        if self._term is not None:
            self._term.write(bytes(data))

    # ---- helpers used by the escape handler ----
    def notice(self, message: str) -> None:
        # Status lines are written on their own CRLF-framed line so they do not
        # corrupt the remote screen.
        if self._term is not None:
            self._term.write(b"\r\n*** " + message.encode() + b"\r\n")
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
            self._console = ipmiconsole.Console(
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
            try:
                while not self._stop:
                    # 1. Pump the IPMI session; delivers server output via
                    #    _on_output. Short timeout keeps input latency low.
                    ipmiconsole.Console.wait_for_rsp(0.05)
                    if self._stop or self._console.broken:
                        break
                    # 2. Forward local keystrokes.
                    data = term.read()
                    if data:
                        outbound = handler.feed(data)
                        if outbound:
                            self._console.send_data(outbound)
            except KeyboardInterrupt:
                # Ctrl-C is forwarded to the remote as a normal keystroke, so we
                # only get here if the terminal failed to deliver it raw.
                pass
            except IpmiException as exc:
                self._error = str(exc)
                rc = 1
            finally:
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
