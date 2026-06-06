# Wiring up Serial-over-LAN on the RD450X (BIOS + bootloader + OS)

`rd450x-console sol` only transports bytes over IPMI. For a real, end-to-end
console — POST, BIOS Setup, the bootloader menu, kernel boot messages, and the
OS login prompt — three layers on the server itself must agree. This guide is the
configuration that makes **all** of them appear over SOL on a Lenovo RD450X (AMI
MegaRAC BMC, firmware 2.36), validated against a Proxmox VE (Debian) host.

The single most important fact: **the SOL port is `COM0`** — labelled
`COM0(SOL)` in the AMI BIOS, which is Linux **`ttyS0` (I/O `0x3F8`)**. It is a
*real hardware UART bridge* to the BMC, not a firmware-redirection-only sink.
`ttyS1`/`0x2F8` is **not** the SOL port — pointing the OS at it will hang the boot
(see Pitfalls).

```
POST ─┐                         ┌─ kernel + getty (OS owns ttyS0)
      ├─ BIOS Setup ─┐          │
      │              ▼          ▼
      │        bootloader menu  │
      └────────────► COM0 / ttyS0 (0x3F8) ◄── BMC SOL bridge ──IPMI RMCP+──► rd450x-console
        BIOS owns the UART      hand-off
        through the loader      "BootLoader"
```

---

## Layer 1 — BIOS (`COM0(SOL)`)

In AMI Setup, under *Console Redirection* (or *Serial Port Sharing*), for the
**`COM0(SOL)`** port:

| Setting | Value |
|---------|-------|
| Console Redirection | **Enabled** |
| **Redirection After BIOS POST** | **BootLoader** |
| Baud rate | 115200 |
| Data / Parity / Stop | 8 / None / 1 |
| Flow Control | None |
| Terminal Type | VT-UTF8 |

Why `Redirection After BIOS POST = BootLoader` and **not** `Always Enable`:
in `Always Enable`, the BIOS keeps holding the UART (via SMM) during OS runtime
and blocks the OS↔BMC path — the OS can write to `/dev/ttyS0` but **0 bytes**
reach SOL. `BootLoader` redirects POST **and** the bootloader, then **releases**
the UART so the OS can own it. (This was the original "SOL is silent" red herring:
activation succeeded but the BIOS never let go of the port.)

---

## Layer 2 — Bootloader (GRUB, EFI)

On Proxmox VE the live GRUB config is `/boot/grub/grub.cfg` (GRUB-EFI). It is
generated from `/etc/default/grub`; edit that, then run `update-grub`. (This host
uses plain GRUB-EFI, **not** `proxmox-boot-tool` — no ESP sync step is needed.
Back up `/etc/default/grub` first.)

```sh
# /etc/default/grub  (the deployed, working values)
GRUB_CMDLINE_LINUX="console=ttyS0,115200n8 console=tty1"
GRUB_CMDLINE_LINUX_DEFAULT="quiet systemd.show_status=yes"
GRUB_TERMINAL="console"
# Harmless here: GRUB_SERIAL_COMMAND is defined but inert because `serial` is not
# in GRUB_TERMINAL, so GRUB's own serial reader is never activated (see the
# double-read warning below). Leaving it set does nothing.
GRUB_SERIAL_COMMAND="serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1"
```

Then:

```sh
update-grub
```

Rationale for each line:

- **`console=ttyS0,115200n8 console=tty1`** — list **both** consoles so they both
  receive kernel boot messages (`/sys/class/tty/console/active` then shows
  `ttyS0 tty1`). The login prompt on SOL does **not** depend on which one the
  kernel picks as `/dev/console`, because `serial-getty@ttyS0` is enabled
  explicitly in Layer 3; keeping `tty1` in the list means the local VGA console
  keeps working too (serial is an *alternative*, not a replacement). The exact
  ordering is not critical here — this is the order the working host uses.
- **`GRUB_CMDLINE_LINUX_DEFAULT="quiet systemd.show_status=yes"`** — `quiet` hides
  the noisy kernel dmesg flood (real errors still print); `systemd.show_status=yes`
  forces **all** systemd `[ OK ]` / `[FAILED]` lines back (plain `quiet` lets
  systemd's `auto` mode hide the `[ OK ]` lines, so boot looks dead).
- **`GRUB_TERMINAL="console"`** — show the GRUB **menu** on both VGA and serial.
  Default Proxmox GRUB draws `gfxterm` (graphics), which is invisible over serial.
  Setting `GRUB_TERMINAL` to a non-`gfxterm` value forces the EFI **text** console,
  which the firmware's BootLoader redirection mirrors to SOL *and* still shows on
  VGA.

> **Do not** also enable GRUB's own `serial` module (i.e. `GRUB_TERMINAL="console
> serial"` + `GRUB_SERIAL_COMMAND`) while BIOS redirection is `BootLoader`. Then
> *two* readers — GRUB's serial driver and the firmware — both read `0x3F8`,
> tearing apart `ESC[A`-style arrow-key sequences. The symptom is erratic menu
> navigation that randomly drops into the `grub>` shell or the entry editor. Let
> the firmware own the UART (single reader, clean key decode): `GRUB_TERMINAL="console"`.
>
> **Plan B** if your firmware refuses to mirror the EFI text console: keep
> `GRUB_TERMINAL="console serial"` (+ `GRUB_SERIAL_COMMAND="serial --unit=0
> --speed=115200"`) but set BIOS **Redirection After BIOS POST = Disabled**, so
> GRUB alone owns the UART during the loader stage.

---

## Layer 3 — OS (serial getty + baud)

Enable a login prompt on the serial console and pin its baud rate:

```sh
systemctl enable --now serial-getty@ttyS0.service
```

**Pin the baud rate** — this matters. The stock `serial-getty@ttyS0.service` runs
`agetty --keep-baud 115200,57600,38400,9600`, and agetty will **cycle down** that
list (e.g. to 38400) while the BMC SOL bridge stays fixed at 115200. The result is
a framing mismatch: `?????` garbage at the login prompt, each Enter returning a
single invalid byte. Confirm with `stty -F /dev/ttyS0` (it shows e.g. `speed 38400
baud` despite `115200` everywhere else).

Fix with a drop-in that forces a single rate:

```sh
# /etc/systemd/system/serial-getty@ttyS0.service.d/override.conf
[Service]
ExecStart=
ExecStart=-/sbin/agetty -o '-- \u' --noreset --noclear 115200 - ${TERM}
```

```sh
systemctl daemon-reload
systemctl restart serial-getty@ttyS0
stty -F /dev/ttyS0        # verify: speed 115200 baud
```

---

## Verify

```sh
rd450x-console sol --info     # firmware/power + BMC SOL channel config (bit rate)
rd450x-console sol            # connect, then press Enter
```

You should be able to reboot the host and watch POST → the BIOS Setup UI
(VT-UTF8) → the GRUB menu → kernel boot → `login:`, all over SOL.

---

## Pitfalls & recovery

- **`console=ttyS1` bricks the boot.** `ttyS1`/`0x2F8` is not the SOL port, and in
  BIOS `BootLoader` mode the firmware disables it after the loader. A kernel
  `console=ttyS1` then hangs at *"Loading initial ramdisk"*. **Recover:** at the
  GRUB menu press `e`, delete the `console=ttyS1…` token, `Ctrl-X` to boot once,
  then fix `/etc/default/grub` + `update-grub`. **Use `ttyS0` only.**
- **`?????` at the login prompt** = host-side **baud drift**, not the client. Pin
  the getty rate (Layer 3). The SOL client only transports bytes — garbage on the
  line means a baud/getty mismatch on the host, never a missing client-side
  charset decoder. Use `rd450x-console sol --debug` to tap the raw inbound bytes
  (`%q` + hex) when diagnosing.
- **Erratic GRUB nav / drops to `grub>`** = UART double-read contention. Use
  `GRUB_TERMINAL="console"`, not `"console serial"`, under BIOS BootLoader
  redirection (Layer 2).
- **"SOL Session active for another client"** = a stale session is held on the
  BMC. Reconnect with `rd450x-console sol --force` to take it over.
