# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project goal

Reimplement the **remote serial console (Serial-over-LAN)** for a Lenovo RD450X
server on a modern **Go** stack, replacing the legacy Java Web Start client
(`jviewer.jnlp`). The existing client requires an ancient JRE (`j2se 1.7+`) and
disabled JVM security (`<all-permissions/>`), which is why it is being ported.

The client is implemented in Go: a single `rd450x-console` binary with `sol`
(serial console) and `kvm` (video) subcommands. An earlier Python prototype was
removed once the Go port reached parity.

**Scope & order of work:** the KVM/video client (`JViewer.jar`) must also be
ported, but **only after** the serial console (`JViewer-SOC.jar`) is implemented.
Serial console first; KVM/video second.

## The target system (from `jviewer.jnlp`)

The RD450X uses an **AMI MegaRAC BMC** (vendor "American Megatrends, Inc.",
client name "JViewer"). Key facts extracted from the JNLP:

- **BMC host:** `192.168.1.90`, web/IPMI UI on port **80**
- **KVM/console port:** **7582**, with `-kvmsecure 1` (TLS-wrapped AMI protocol)
- **Virtual media ports:** CD `5120`, FD `5122`, HD `5123`
- Auth is session-based: the JNLP carries a `-kvmtoken` and `-webcookie` that the
  web UI mints **per login session**. Those values in `jviewer.jnlp` are stale
  example session tokens — a real client must log into the web UI first and
  obtain fresh ones. Do not hardcode them.
- `JViewer.jar` is the KVM/video client; **`JViewer-SOC.jar` is the serial
  console** module — that is the one relevant to this port. The jars are served
  from `http://192.168.1.90:80/Java/release/`, not stored locally.

Before reverse-engineering the proprietary AMI protocol, **try standard IPMI
Serial-over-LAN first** (`ipmitool -I lanplus -H 192.168.1.90 ... sol activate`)
— most MegaRAC BMCs expose SOL over RMCP+ on UDP 623, which is far simpler than
the JViewer wire protocol. Only fall back to reversing `JViewer-SOC.jar` if the
standard path is unavailable on this BMC.

## Credentials & secret-handling protocol

IPMI web-UI credentials live in `.env` (gitignored), keyed as in
`.env.example`: `IPMI_USER`, `IPMI_PASSWORD`.

**Read `.env` only at runtime, never into the agent's context.** When testing or
debugging, load it inside the program (the Go client reads it via
`internal/config`) — do **not** `cat`/`Get-Content`/`echo` the file or print the
password. Reference the variables by name only.

## Reverse-engineering workflow

Go is already installed. Install RE tooling via **scoop**:

```powershell
scoop install jadx        # or: cfr / procyon — Java decompilers for the .jar files
scoop install ipmitool    # try standard SOL before reversing the proprietary protocol
```

To inspect the protocol: download the served jars
(`http://192.168.1.90:80/Java/release/JViewer-SOC.jar`), decompile, and study the
socket/handshake logic against port 7582. A packet capture of a live JViewer
session against the BMC is the highest-signal reference for the wire format.
