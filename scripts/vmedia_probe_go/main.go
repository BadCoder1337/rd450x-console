// vmedia_probe_go brings up a single AMI IUSB CD-ROM redirection against the live
// BMC and serves a local ISO to the host, with full packet logging. It is the
// bring-up / validation vehicle for internal/kvm/vmedia: it exercises the real
// handshake and lets us read the BMC's actual SCSI request packets in plaintext
// (we terminate the TLS), which is how the request/response layout is confirmed.
//
// It deliberately does ONE web login (the MegaRAC web stack is fragile — see the
// project notes) and releases it on exit. No retry loops.
//
// Usage:  go run ./scripts/vmedia_probe_go [-iso bin/test.iso] [-instance 0]
//
// After it prints "redirection accepted", check the host (ssh root@pve...):
//
//	dmesg | tail            # USB CD-ROM attach
//	ls -l /dev/cdrom        # device node
//	dd if=/dev/cdrom bs=2048 count=24 2>/dev/null | strings | grep RD450X
//
// Loads .env at runtime; never prints the password.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rd450x-console/internal/config"
	"rd450x-console/internal/kvm"
	"rd450x-console/internal/kvm/vmedia"
)

func main() {
	iso := flag.String("iso", "bin/test.iso", "image file to redirect (when -dev is empty)")
	dev := flag.String("dev", "", "redirect a PHYSICAL Windows volume by drive letter (e.g. Y:) instead of -iso; needs Administrator")
	disk := flag.String("disk", "", "redirect a WHOLE physical USB disk (by drive letter like Y: or disk number like 4) instead of -iso/-dev; needs Administrator")
	devType := flag.String("type", "cd", "device type: cd | fd | hd")
	writable := flag.Bool("w", false, "writable: honour host WRITE(10) (fd/hd only)")
	duration := flag.Duration("duration", 0, "auto-stop after this long (0 = run until Ctrl-C); useful for elevated detached runs")
	instance := flag.Int("instance", 0, "device instance/slot")
	quiet := flag.Bool("quiet", false, "disable per-packet hex logging")
	jnlpOnly := flag.Bool("jnlp", false, "just log in and print the vmedia-relevant jnlp args, then exit")
	flag.Parse()

	config.LoadDotEnv(".env")
	creds := config.Load("", "")
	if creds.Host == "" || creds.User == "" || creds.Password == "" {
		fmt.Fprintln(os.Stderr, "missing IPMI_HOST/IPMI_USER/IPMI_PASSWORD (.env)")
		os.Exit(1)
	}

	if *jnlpOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		args, _, err := kvm.FetchLaunchArgs(ctx, creds.Host, creds.User, creds.Password)
		if err != nil {
			log.Fatalf("fetch jnlp: %v", err)
		}
		// Print only non-secret vmedia config (never the token/cookie).
		for _, k := range []string{
			"kvmport", "kvmsecure", "vmsecure", "singleportenabled",
			"cdport", "cdnum", "cdstate", "fdport", "fdnum", "fdstate",
			"hdport", "hdnum", "hdstate", "websecureport", "kvmtokentype",
		} {
			if v, ok := args[k]; ok {
				fmt.Printf("  %-18s = %s\n", k, v)
			}
		}
		return
	}

	// Map the device type to its port + a host-side hint.
	var port int
	var hint string
	switch *devType {
	case "cd":
		port, hint = vmedia.PortCD, "/dev/cdrom (sr0)"
	case "fd":
		port, hint = vmedia.PortFD, "the AMI 'Virtual Floppy0' /dev/sdX"
	case "hd":
		port, hint = vmedia.PortHD, "the AMI 'Virtual HDisk0' /dev/sdX"
	default:
		log.Fatalf("invalid -type %q (want cd|fd|hd)", *devType)
	}
	if *writable && *devType == "cd" {
		log.Fatalf("-w (writable) is not valid for a CD-ROM; use -type fd or hd")
	}

	// Open the backing: a physical volume (-dev) or an image file (-iso).
	var (
		reader  vmedia.Reader
		rw      vmedia.ReadWriter // non-nil when writable
		closer  io.Closer
		srcName string
	)
	if *disk != "" {
		// Whole physical disk: accept a disk number ("4") or a drive letter ("Y:").
		n, err := strconv.Atoi(strings.TrimRight(*disk, `:\`))
		if err != nil {
			n, err = vmedia.DriveLetterToDisk(*disk)
			if err != nil {
				log.Fatalf("resolve disk for %s: %v", *disk, err)
			}
			log.Printf("drive %s is on physical disk %d", *disk, n)
		}
		vol, err := vmedia.OpenPhysicalDrive(n, *writable)
		if err != nil {
			log.Fatalf("open physical disk %d: %v", n, err)
		}
		reader, closer, srcName = vol, vol, fmt.Sprintf("PhysicalDrive%d", n)
		if *writable {
			rw = vol
		}
		log.Printf("WHOLE DISK PhysicalDrive%d: %d bytes (%d MiB)%s", n, vol.Size(), vol.Size()>>20,
			map[bool]string{true: " — WRITABLE (disk taken offline)", false: " — read-only"}[*writable])
	} else if *dev != "" {
		vol, err := vmedia.OpenVolume(*dev, *writable)
		if err != nil {
			log.Fatalf("open volume %s: %v", *dev, err)
		}
		reader, closer, srcName = vol, vol, *dev
		if *writable {
			rw = vol
		}
		log.Printf("PHYSICAL volume %s: %d bytes (%d MiB)%s", *dev, vol.Size(), vol.Size()>>20,
			map[bool]string{true: " — WRITABLE (locked+dismounted)", false: " — read-only"}[*writable])
	} else {
		var fr *vmedia.FileReader
		var err error
		if *writable {
			fr, err = vmedia.OpenFileRW(*iso)
		} else {
			fr, err = vmedia.OpenFile(*iso)
		}
		if err != nil {
			log.Fatalf("open image: %v", err)
		}
		reader, closer, srcName = fr, fr, *iso
		if *writable {
			rw = fr
		}
	}
	defer closer.Close()
	log.Printf("serving %s as %s (%d bytes)", srcName, *devType, reader.Size())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	// One web login → STOKEN for the vmedia auth. Released on exit.
	loginCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, err := kvm.Login(loginCtx, creds.Host, creds.User, creds.Password)
	cancel()
	if err != nil {
		log.Fatalf("web login: %v", err)
	}
	defer kvm.Logout(creds.Host, ws.Cookie)
	log.Printf("web login OK, got session token (%d chars)", len(ws.Token))

	sess, err := vmedia.Connect(ctx, vmedia.Options{
		Host:       creds.Host,
		Port:       port,
		DeviceType: vmedia.DeviceCDROM, // JViewer uses the CD header for FD/HD auth too; the port selects the device
		Instance:   uint8(*instance),
		Debug:      !*quiet,
	}, ws.Token)
	if err != nil {
		log.Fatalf("vmedia connect: %v", err)
	}
	defer sess.Close()

	var emu *vmedia.Device
	switch {
	case *devType == "cd":
		emu = vmedia.NewCDROM(reader)
	case rw != nil:
		emu = vmedia.NewDiskRW(rw)
	default:
		emu = vmedia.NewDisk(reader)
	}

	log.Printf("serving SCSI; Ctrl-C to stop. Check %s on the host now.", hint)
	if err := sess.Serve(ctx, emu); err != nil && ctx.Err() == nil {
		log.Printf("session ended: %v", err)
	}
	log.Printf("done; %d bytes transferred", emu.BytesServed())
}
