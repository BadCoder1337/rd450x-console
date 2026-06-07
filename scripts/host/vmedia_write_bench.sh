#!/usr/bin/env bash
# vmedia_write_bench.sh — measure sequential WRITE throughput into an AMI virtual
# disk redirected via rd450x-console vmedia (Go path or browser path).
#
# *** DESTRUCTIVE ***: this script overwrites the first 100 MiB of the target
# device with zeros. Use it ONLY against the AMI Virtual HDisk — NEVER against
# a real disk. Multiple safety checks enforce this.
#
# Safety rules (all must pass, or the script aborts):
#   1. Requires explicit --yes flag on the command line.
#   2. Identifies the device ONLY by SCSI identity — vendor "AMI" + model
#      "Virtual HDisk" (/sys/block/*/device/{vendor,model}) — never hardcodes /dev/sdX.
#   3. Refuses to run if the device cannot be found unambiguously.
#   4. Re-reads and re-checks the model immediately before dd executes.
#   5. Refuses if the model re-check fails.
#
# Usage (on the Proxmox host, as root):
#   bash /root/vmedia_write_bench.sh --yes
#
# Run AFTER starting the vmedia probe on the Windows/client side with -w flag:
#   go run ./scripts/vmedia_probe_go -iso bin/bench-write-100m.img -type hd -w

set -euo pipefail

# ---------------------------------------------------------------------------
# Guard: require explicit --yes
# ---------------------------------------------------------------------------

CONFIRMED=0
for arg in "$@"; do
    [[ "$arg" == "--yes" ]] && CONFIRMED=1
done

if [[ $CONFIRMED -eq 0 ]]; then
    echo "ERROR: This script writes to a block device. You must pass --yes to confirm." >&2
    echo "       Usage: bash $0 --yes" >&2
    echo ""
    echo "       ONLY run this against the AMI Virtual HDisk redirected by vmedia_probe_go." >&2
    exit 1
fi

COUNT_MB=100

# ---------------------------------------------------------------------------
# Device identification
# ---------------------------------------------------------------------------

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

echo "=== vmedia WRITE benchmark (DESTRUCTIVE) ==="
echo "Looking for AMI Virtual HDisk device..."

mapfile -t devs < <(find_ami_device | tr ' ' '\n' | grep -v '^$' || true)

if [[ ${#devs[@]} -eq 0 ]]; then
    echo "ERROR: No AMI Virtual HDisk found in /sys/block/*/device/model" >&2
    echo "       Is vmedia_probe_go running on the client with -w flag? Is -type hd mounted?" >&2
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
# Second safety check: re-read model JUST BEFORE dd
# ---------------------------------------------------------------------------
VENDOR_CHECK=$(tr -d '[:space:]' < "/sys/block/${devs[0]}/device/vendor" 2>/dev/null)
MODEL_CHECK=$(tr -d '[:space:]' < "/sys/block/${devs[0]}/device/model" 2>/dev/null)
if [[ "$VENDOR_CHECK" != *AMI* || "$MODEL_CHECK" != *VirtualHDisk* ]]; then
    echo "ERROR: Pre-write identity re-check failed: got vendor='$VENDOR_CHECK' model='$MODEL_CHECK'" >&2
    echo "       Device identity changed or sysfs path stale. Aborting." >&2
    exit 1
fi

echo "Pre-write identity check: OK (vendor=$VENDOR_CHECK model=$MODEL_CHECK)"
echo ""

# ---------------------------------------------------------------------------
# Benchmark: dd with direct I/O + fdatasync
# ---------------------------------------------------------------------------
echo "--- dd direct I/O write + fdatasync ---"
echo "Command: dd if=/dev/zero of=$DEV bs=1M count=$COUNT_MB oflag=direct conv=fdatasync"
echo ""
echo "Writing ${COUNT_MB} MiB of zeros to $DEV ..."
echo ""
dd if=/dev/zero of="$DEV" bs=1M count="$COUNT_MB" oflag=direct conv=fdatasync 2>&1
echo ""

# ---------------------------------------------------------------------------
# Benchmark 2: buffered write (fills page cache, measures apparent throughput)
# ---------------------------------------------------------------------------
# Note: buffered writes without fdatasync measure write-back to the block layer
# which is often much higher than the actual network/USB path. We include it for
# completeness but note the caveat.
echo "--- dd buffered write (no oflag=direct, with conv=fdatasync) ---"
echo "Command: dd if=/dev/zero of=$DEV bs=1M count=$COUNT_MB conv=fdatasync"
echo ""
dd if=/dev/zero of="$DEV" bs=1M count="$COUNT_MB" conv=fdatasync 2>&1
echo ""

echo "=== WRITE benchmark complete ==="
echo "NOTE: the first ${COUNT_MB} MiB of the image on the client side now contains zeros."
echo "      Restart vmedia_probe_go to serve a fresh image if needed."
