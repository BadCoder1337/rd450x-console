# RD450X KVM (JViewer) protocol — reverse-engineering notes

Findings from decompiling the BMC-served `JViewer.jar` + `JViewer-SOC.jar`
(AMI MegaRAC, firmware 2.36, ASPEED AST SoC). This is the **video/keyboard/mouse
(KVM)** path — phase 2 of the project, after the working SOL serial console.

> The jars and decompiled sources are AMI-copyrighted and live under `re/`
> (gitignored). This file is an independent description of the wire format for
> a clean-room reimplementation.

## TL;DR

- **Transport:** one TCP socket to `192.168.1.90:7582`. With `kvmsecure=1` it is
  wrapped in TLS (client trusts any cert — `InsecureSkipVerify`). `singleportenabled=0`
  here, so it's a direct connection to 7582 (no 80/443 tunnelling).
- **Auth:** no password on the wire. The web UI mints a **session token** that is
  presented to the video port. Same two-step HTTP login as the SOL bring-up:
  1. `POST /rpc/WEBSES/create.asp` (`WEBVAR_USERNAME`,`WEBVAR_PASSWORD`) → `SESSION_COOKIE`
  2. `GET /rpc/getsessiontoken.asp` (Cookie: `SessionCookie=…`) → `STOKEN`
  `STOKEN` == the JNLP `-kvmtoken`; `SESSION_COOKIE` == `-webcookie`.
- **Framing:** every message starts with an 8-byte **IVTP header**, little-endian.
- **Video codec:** AMI/ASPEED hardware codec — a hybrid **VQ + JPEG (DCT)** tile
  stream, YUV 4:2:0, optionally **RC4-encrypted**. Full decoder logic is in
  `JViewer-SOC.jar` → `…/soc/video/Decoder.java` (~67 KB) + `JTables`,
  `HuffmanTable`, `VideoHeader`. This is the hard part of any port.

## IVTP packet header (8 bytes, little-endian)

`com.ami.kvm.jviewer.kvmpkts.IVTPPktHdr`

| Offset | Size | Field    | Notes                                    |
|--------|------|----------|------------------------------------------|
| 0      | 2    | `type`   | u16 opcode (table below)                 |
| 2      | 4    | `pktSize`| u32 payload size following the header    |
| 6      | 2    | `status` | u16 — command-specific status/sub-opcode |

### Opcodes (`type`) actually used

| Val | Name                          | Direction | Purpose |
|-----|-------------------------------|-----------|---------|
| 1   | `HID_PKT`                     | C→S | USB keyboard/mouse report |
| 2   | `SET_BANDWIDTH`               | C→S | |
| 3   | `SET_FPS`                     | C→S | |
| 4   | `PAUSE_REDIRECTION`           | C→S | |
| 5   | `REFRESH_VIDEO_SCREEN`        | C→S | full-screen refresh request |
| 6   | `RESUME_REDIRECTION`          | C→S | sent after session validates → starts video |
| 7   | `SET_COMPRESSION_TYPE`        | C→S | |
| 8   | `STOP_SESSION_IMMEDIATE`      | both | `status` = stop reason |
| 9   | `BLANK_SCREEN`                | S→C | host video blanked → show "no signal" |
| 11  | `GET_FULL_SCREEN`             | C→S | |
| 12/13 | `ENABLE/DISABLE_ENCRYPTION`| both | toggle RC4 on the stream |
| 14/15 | `ENCRYPTION_STATUS`/`INITIAL_ENCRYPTION_STATUS` | S→C | |
| 16/17 | `BW_DETECT_REQ/RESP`       | both | bandwidth probe |
| 18  | `VALIDATE_VIDEO_SESSION`      | C→S | **handshake** — presents session token (below) |
| 19  | `VALIDATE_VIDEO_SESSION_RESPONSE` | S→C | `status`/byte0 = `1` VALID, `0` invalid |
| 20  | `GET_KEYBD_LED`               | both | LED state |
| 23  | `SESSION_ACCEPTED`            | S→C | active-clients list (48-byte records) |
| 24  | `MEDIA_STATE`                 | C→S | |
| 25  | `VIDEO_FRAGMENT`              | S→C | **video data** (fragmented frame, see below) |
| 28  | `SET_MOUSE_MODE`              | both | absolute/relative |
| 32  | `KVM_SHARING`                 | both | multi-user privilege |
| 34/35/36 | `POWER_STATUS` / `POWER_CONTROL_REQUEST/RESPONSE` | both | chassis power |
| 40/41 | `GET/SET_USER_MACRO`        | both | |
| 48/49 | `IPMI_REQUEST/RESPONSE_PKT` | both | tunnelled IPMI |
| 57  | `KEEP_ALIVE_PKT`              | C→S | heartbeat |
| 58  | `CONNECTION_COMPLETE_PKT`     | C→S | used on reconnect |

## Handshake

```
TCP connect 7582  (TLS if kvmsecure=1, trust-all cert)
        │
C→S  IVTP type=18 VALIDATE_VIDEO_SESSION, pktSize=373  (body layout below)
S→C  IVTP type=19 VALIDATE_VIDEO_SESSION_RESPONSE, byte0 = 1 (VALID_SESSION)
        │
C→S  IVTP type=6  RESUME_REDIRECTION
S→C  IVTP type=25 VIDEO_FRAGMENT … (frames begin)
C→S  IVTP type=57 KEEP_ALIVE every few seconds
```

### type=18 body (373 bytes, the `VIDEO_PACKET_SIZE`)

Built in `JViewerApp.OnsendWebsessionToken()`. All string fields are
zero-padded fixed-width:

| Offset (in body) | Size | Field |
|------|------|-------|
| 0    | 1    | token type: `0` = WEB_SESSION_TOKEN, `1` = SSI |
| 1    | 129  | session token (`STOKEN`) ASCII + zero pad (130-byte block incl. type byte) |
| 130  | 65   | client own IP, ASCII + zero pad |
| 195  | 129  | client username, ASCII + zero pad |
| 324  | 49   | client MAC (`aa-bb-…`), ASCII + zero pad |

(The reconnect path, `onReconnect()`, uses type=58 with an extra trailing
session-id byte.)

## Video fragments (type 25)

Frames are split into fragments (`MAX_FRAGMENT_SIZE = 4_608_000`). The reader
chain reassembles: `HeaderReader → FragNumReader → FragReader`. Each reassembled
frame begins with a **frame header** (`SOCFrameHdr`/`VideoHeader`) carrying
resolution (`resX`,`resY`), frame type, and compression/quant selectors, then the
compressed tile data.

### Codec (`Decoder.java`)

The ASPEED video engine output — a tile-based hybrid:

- Screen split into 16×16 (luma) macro-tiles; per-tile **block header** selects:
  - `JPEG_BLOCK` — DCT + Zig-zag + quant (`JTables`) + Huffman (`HuffmanTable`,
    DC/AC tables ×4), YUV→RGB via precomputed tables. Standard baseline-JPEG-ish
    math with AMI's own quant/Huffman tables.
  - `VQ_BLOCK` — vector-quantization: 1/2/4-entry 24-bit color codebook
    (`VQ_COLOR_MASK = 0xFFFFFF`), for flat/low-detail tiles.
  - `SKIP` codes — tile unchanged from previous frame (delta coding;
    `previousYUVData` retained).
- `m_Mode420` — YUV 4:2:0 chroma subsampling.
- **RC4 layer:** `Decoder` holds an `rc4_state`; `DecodeKeys = "fedcba9876543210"`.
  When stream encryption is on (opcodes 12/14/15), tile data is RC4-decrypted
  before entropy decode. Encryption can also be disabled per session.

This decoder is ~2k lines of bit-level work and is the single largest porting
task. It is fully deterministic and translatable line-for-line.

## Keyboard / mouse (HID, type 1)

`com.ami.kvm.jviewer.hid.*` — standard **USB HID reports** (8-byte keyboard
report, mouse report with absolute or relative coords). Layouts map host keys to
USB usage codes (`USBKeyProcessor*`). Reports may be RC4-encrypted via `KMCrypt`
when KM encryption is negotiated; otherwise sent in clear inside a type=1 IVTP
packet. Power control is type=35 with `status` = action
(`0` off / `1` on / `2` cycle / `3` hard reset / `5` soft reset).

## Notes for a reimplementation

- TLS: Java uses `SSLContext.getInstance("SSL")` — the BMC may only speak old
  TLS/ciphers. A Go client likely needs `MinVersion: tls.VersionTLS10` (or lower)
  plus a permissive `CipherSuites` set, and `InsecureSkipVerify: true`.
- Endianness is **little-endian** throughout.
- The session token expires with the web session; mint it fresh each launch and
  keep the web session alive (the SOL client already does the login dance).
- A passive `tcpdump`/Wireshark capture of one real JViewer session is still the
  best ground-truth to validate the decoder against, frame for frame.
