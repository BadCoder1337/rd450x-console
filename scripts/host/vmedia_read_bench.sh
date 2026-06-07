#!/usr/bin/env bash
# vmedia_read_bench.sh — measure sequential READ throughput of an AMI virtual disk
# redirected via rd450x-console vmedia (Go path or browser path).
#
# Safety: identifies the device ONLY by SCSI identity — vendor "AMI" + model
# "Virtual HDisk" via /sys/block/*/device/{vendor,model}. Refuses to run if the
# device cannot be found unambiguously. NEVER hardcodes /dev/sdX.
#
# Usage (on the Proxmox host, as root):
#   bash /root/vmedia_read_bench.sh
#
# Requirements: dd (always present), hdparm (optional), blockdev (util-linux).
# Run AFTER starting the vmedia probe on the Windows/client side:
#   go run ./scripts/vmedia_probe_go -iso bin/bench-read-100m.img -type hd

set -euo pipefail

# ---------------------------------------------------------------------------
# Device identification
# ---------------------------------------------------------------------------

COUNT_MB=100

# The host SCSI INQUIRY (answered by the BMC firmware) reports vendor "AMI" and
# product/model "Virtual HDisk0" — the "AMI" lives in /device/vendor, NOT in
# /device/model. Match on vendor=AMI AND model containing "VirtualHDisk" so we
# pick ONLY the virtual hard disk, never the always-present Virtual Floppy0 (0 B)
# or Virtual CDROM0 (sr*), and never a real disk.
find_ami_device() {
    local matches=() dir dev vendor model
    for dir in /sys/block/sd*; do
        [[ -r "$dir/device/model" ]] || continue
        dev=$(basename "$dir")
        vendor=$(tr -d '[:space:]' < "$dir/device/vendor" 2>/dev/null)
        model=$(tr -d '[:space:]' < "$dir/device/model" 2>/dev/null)
        if [[ "$vendor" == *AMI* && "$model" == *VirtualHDisk* ]]; then
            matches+=("$dev")
        fi
    done
    echo "${matches[@]:-}"
}

echo "=== vmedia READ benchmark ==="
echo "Looking for AMI Virtual HDisk device..."

mapfile -t devs < <(find_ami_device | tr ' ' '\n' | grep -v '^$' || true)

if [[ ${#devs[@]} -eq 0 ]]; then
    echo "ERROR: No AMI Virtual HDisk found in /sys/block/*/device/model" >&2
    echo "       Is vmedia_probe_go running on the client? Is the -type hd image mounted?" >&2
    echo "       Check: lsblk -o NAME,MODEL,SIZE" >&2
    exit 1
fi

if [[ ${#devs[@]} -gt 1 ]]; then
    echo "ERROR: Multiple AMI Virtual HDisk devices found: ${devs[*]}" >&2
    echo "       Refusing to continue — detach one before benchmarking." >&2
    exit 1
fi

DEV="/dev/${devs[0]}"
MODEL=$(cat "/sys/block/${devs[0]}/device/model" | tr -d '\n')
SIZE_BYTES=$(blockdev --getsize64 "$DEV" 2>/dev/null || echo "?")

echo "Device : $DEV"
echo "Model  : $MODEL"
echo "Size   : $SIZE_BYTES bytes"
echo ""

# Sanity: confirm size is at least COUNT_MB MiB
if [[ "$SIZE_BYTES" =~ ^[0-9]+$ ]] && (( SIZE_BYTES < COUNT_MB * 1024 * 1024 )); then
    echo "ERROR: Device is smaller than ${COUNT_MB} MiB — wrong image mounted?" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Drop caches
# ---------------------------------------------------------------------------
echo "Dropping page cache / buffer cache..."
sync
echo 3 > /proc/sys/vm/drop_caches

# ---------------------------------------------------------------------------
# Benchmark 1: dd direct I/O (avoids page cache on the host side)
# ---------------------------------------------------------------------------
echo ""
echo "--- dd direct I/O (iflag=direct) ---"
echo "Command: dd if=$DEV of=/dev/null bs=1M count=$COUNT_MB iflag=direct"
echo ""
dd if="$DEV" of=/dev/null bs=1M count="$COUNT_MB" iflag=direct 2>&1
echo ""

# ---------------------------------------------------------------------------
# Benchmark 2: dd buffered (page cache warm; shows cache/memcpy ceiling)
# ---------------------------------------------------------------------------
echo "Dropping caches again before buffered read..."
sync
echo 3 > /proc/sys/vm/drop_caches
echo ""
echo "--- dd buffered (no iflag=direct) ---"
echo "Command: dd if=$DEV of=/dev/null bs=1M count=$COUNT_MB"
echo ""
dd if="$DEV" of=/dev/null bs=1M count="$COUNT_MB" 2>&1
echo ""

# ---------------------------------------------------------------------------
# Benchmark 3: hdparm -t (if available)
# ---------------------------------------------------------------------------
if command -v hdparm &>/dev/null; then
    echo "--- hdparm -t (timing buffered disk reads) ---"
    hdparm -t "$DEV" 2>&1 || true
    echo ""
else
    echo "(hdparm not installed — skipping hdparm -t)"
    echo ""
fi

echo "=== READ benchmark complete ==="
