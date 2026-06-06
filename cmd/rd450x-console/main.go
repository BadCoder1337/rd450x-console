// Command rd450x-console is a single-binary bridge to a Lenovo RD450X BMC.
//
// It has two modes:
//
//	rd450x-console kvm   – start a local web server that serves an embedded
//	                       noVNC client; the binary acts as a VNC (RFB) server
//	                       that bridges to the BMC's proprietary IVTP/ASPEED
//	                       KVM protocol on TCP 7582.
//	rd450x-console sol   – interactive Serial-over-LAN console (IPMI 2.0 RMCP+).
//
// Credentials come from the environment only (IPMI_USER / IPMI_PASSWORD); the
// password is never taken from the command line nor logged.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"rd450x-console/internal/config"
	"rd450x-console/internal/kvm"
	"rd450x-console/internal/sol"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `rd450x-console %s – RD450X BMC bridge

usage:
  rd450x-console kvm [flags]   open the KVM/video console in a browser (noVNC)
  rd450x-console sol [flags]   open the Serial-over-LAN console in the terminal

Run "rd450x-console <mode> -h" for mode-specific flags.
Credentials are read from IPMI_USER / IPMI_PASSWORD (or .env).
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// Load .env (if present) into the environment without ever printing it.
	config.LoadDotEnv(".env")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "kvm":
		err = kvm.RunCommand(ctx, os.Args[2:])
	case "sol":
		err = sol.RunCommand(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	case "-v", "--version", "version":
		fmt.Printf("rd450x-console %s\n", version)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
