# vmedia benchmark scripts (host side)

These scripts run on the **Proxmox host** (or any Linux system where the AMI
virtual disk appears as a block device). They measure the sequential read and
write throughput of the AMI IUSB virtual-media data plane.

Both scripts identify the target device **only by its MODEL string**
(`AMI Virtual HDisk`). They will refuse to run if the device is not found or if
there is any ambiguity, so they are safe to run on a live server.

## Prerequisites

- The `rd450x-console` binary (or `go run ./scripts/vmedia_probe_go`) running on
  the client machine with the benchmark image mounted.
- Root access on the Proxmox host (required for `echo 3 > /proc/sys/vm/drop_caches`
  and direct-I/O writes to a block device).
- The two benchmark images in `bin/` (generate them with `mkbench_go` — see below).

## Generating the benchmark images (on the client / Windows machine)

```sh
# 100 MiB random-content image for READ benchmarks
go run ./scripts/mkbench_go -out bin/bench-read-100m.img -size 100MiB -mode random

# 100 MiB zero-filled image for WRITE benchmarks
go run ./scripts/mkbench_go -out bin/bench-write-100m.img -size 100MiB -mode zero
```

Both produce exactly **104857600 bytes** (100 × 2^20). The random image uses a
fixed PRNG seed so the output is byte-for-byte reproducible across platforms.

## Deploying the scripts to the host

```sh
scp scripts/host/vmedia_read_bench.sh  root@<pve-host>:/root/
scp scripts/host/vmedia_write_bench.sh root@<pve-host>:/root/
ssh root@<pve-host> "chmod +x /root/vmedia_read_bench.sh /root/vmedia_write_bench.sh"
```

Replace `<pve-host>` with the actual hostname or IP of the Proxmox server (e.g.
`192.168.1.X` — check your project's `.env` `IPMI_HOST` or the Proxmox web UI).

## Full benchmark workflow

### READ benchmark (Go turbo path)

1. **Client** — serve the read image via the Go file-backed path (no browser):
   ```sh
   go run ./scripts/vmedia_probe_go -iso bin/bench-read-100m.img -type hd
   ```
   Wait for `redirection accepted` and the host to see the device
   (`dmesg | tail` should show a new SCSI disk).

2. **Host** — run the read benchmark:
   ```sh
   bash /root/vmedia_read_bench.sh
   ```
   The script drops caches, runs `dd if=<dev> of=/dev/null bs=1M count=100
   iflag=direct`, and prints MB/s. It also runs a buffered variant and
   `hdparm -t` if available.

3. Note the **Go path MB/s** figure.

### READ benchmark (browser path)

1. **Client** — start the KVM console:
   ```sh
   ./bin/rd450x-console kvm
   ```
   The browser opens at `http://127.0.0.1:6080/vnc.html`.

2. In the **Virtual Media panel** (toolbar), click "Choose file", select
   `bin/bench-read-100m.img`, set type to **HD**, and click **Mount**.
   Wait for the host to detect the device.

3. **Host** — run the same script:
   ```sh
   bash /root/vmedia_read_bench.sh
   ```
   Note the **browser path MB/s** figure and compare to the Go path.

### WRITE benchmark (Go turbo path)

1. **Client** — serve the write image writable:
   ```sh
   go run ./scripts/vmedia_probe_go -iso bin/bench-write-100m.img -type hd -w
   ```

2. **Host** — run the write benchmark (requires `--yes`):
   ```sh
   bash /root/vmedia_write_bench.sh --yes
   ```
   The script writes 100 MiB of zeros with `dd oflag=direct conv=fdatasync`
   and prints MB/s.

3. Note the **Go path write MB/s**.

### WRITE benchmark (browser path)

1. **Client** — start the KVM console and mount `bin/bench-write-100m.img` as
   HD, **writable** (requires the File System Access API — Chrome/Edge; tick the
   "Writable" checkbox in the Virtual Media panel).

2. **Host** — run the write benchmark:
   ```sh
   bash /root/vmedia_write_bench.sh --yes
   ```

3. Compare **browser path write MB/s** to the Go path.

## Interpreting results

| Path | Expected limiting factor |
|------|--------------------------|
| Go turbo (read) | TCP socket to BMC (5123), BMC USB emulation, PCIe/USB path on host |
| Browser (read)  | All of the above + WebSocket round-trip for each 128 KiB request |
| Go turbo (write)| Same TCP path; host write data travels back to Go, Go writes to image file |
| Browser (write) | WebSocket round-trip per write request; FSA commit only at unmount |

The browser path adds one extra network round-trip per 128 KiB request (the
Go binary fetches data from the browser over `/control` before forwarding it to
the BMC). Enabling the planned LRU read-ahead cache in `internal/kvm/vmedia`
should close this gap significantly for sequential reads.
