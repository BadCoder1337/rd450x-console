// Package sol provides the Serial-over-LAN console mode of the unified binary.
//
// SOL is currently implemented by the Python client in src/rd450x_console (IPMI
// 2.0 RMCP+ via pyghmi). This Go port — planned on github.com/bougou/go-ipmi —
// will bring SOL into the single binary alongside KVM. Until then this command
// points the user at the working client.
package sol

import (
	"context"
	"flag"
	"fmt"
)

// RunCommand implements `rd450x-console sol`.
func RunCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sol", flag.ContinueOnError)
	_ = fs.String("host", "", "BMC host (default: IPMI_HOST or built-in)")
	_ = fs.String("user", "", "BMC user (default: IPMI_USER)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("SOL-in-Go not yet ported; use the working client meanwhile: `rd450x-console` (Python, src/rd450x_console)")
}
