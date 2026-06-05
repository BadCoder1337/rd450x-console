// Package config loads BMC connection settings and credentials.
//
// Per the project's secret-handling rule, the password is sourced exclusively
// from the IPMI_PASSWORD environment variable and is never logged or echoed.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Creds holds the resolved BMC endpoint and credentials.
type Creds struct {
	Host     string
	User     string
	Password string // never logged
}

// Defaults.
const (
	DefaultHost = "192.168.1.90"
)

// LoadDotEnv reads a dotenv-style file and sets any keys not already present in
// the environment. Missing files are ignored. Values are never printed.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}

// Load resolves credentials, applying the host override if non-empty.
func Load(hostOverride, userOverride string) Creds {
	host := firstNonEmpty(hostOverride, os.Getenv("IPMI_HOST"), DefaultHost)
	user := firstNonEmpty(userOverride, os.Getenv("IPMI_USER"))
	return Creds{
		Host:     host,
		User:     user,
		Password: os.Getenv("IPMI_PASSWORD"),
	}
}

// PortOr returns IPMI_PORT as an int, or def if unset/invalid.
func PortOr(def int) int {
	if v := os.Getenv("IPMI_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
