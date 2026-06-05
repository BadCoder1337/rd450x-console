package kvm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WebSession is the result of logging into the BMC web UI. The token is
// presented to the video port to validate the KVM session.
type WebSession struct {
	Token  string // STOKEN — the JNLP -kvmtoken
	Cookie string // SESSION_COOKIE — the JNLP -webcookie
}

var (
	reCookie = regexp.MustCompile(`'SESSION_COOKIE'\s*:\s*'([^']*)'`)
	reToken  = regexp.MustCompile(`'(?:STOKEN|SESSION_TOKEN)'\s*:\s*'([^']*)'`)
)

// Login performs the two-step MegaRAC web authentication:
//
//  1. POST /rpc/WEBSES/create.asp  → SESSION_COOKIE
//  2. GET  /rpc/getsessiontoken.asp (with the cookie) → STOKEN
//
// The password is sent over the BMC HTTP session only and is never logged.
func Login(ctx context.Context, host, user, password string) (WebSession, error) {
	hc := &http.Client{Timeout: 15 * time.Second}
	base := "http://" + host

	// Step 1: create web session.
	form := url.Values{"WEBVAR_USERNAME": {user}, "WEBVAR_PASSWORD": {password}}
	body, err := httpDo(ctx, hc, http.MethodPost, base+"/rpc/WEBSES/create.asp",
		strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return WebSession{}, fmt.Errorf("web login: %w", err)
	}
	m := reCookie.FindStringSubmatch(body)
	if m == nil {
		return WebSession{}, fmt.Errorf("web login: no SESSION_COOKIE in response (bad credentials?)")
	}
	cookie := m[1]

	// Step 2: mint the KVM session token.
	body, err = httpDo(ctx, hc, http.MethodGet, base+"/rpc/getsessiontoken.asp", nil,
		map[string]string{"Cookie": "SessionCookie=" + cookie})
	if err != nil {
		return WebSession{}, fmt.Errorf("get session token: %w", err)
	}
	m = reToken.FindStringSubmatch(body)
	if m == nil {
		return WebSession{}, fmt.Errorf("get session token: no STOKEN in response")
	}
	return WebSession{Token: m[1], Cookie: cookie}, nil
}

func httpDo(ctx context.Context, hc *http.Client, method, u string, body io.Reader, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), nil
}

// buildValidatePacket assembles the IVTP type=18 VALIDATE_VIDEO_SESSION packet:
// an 8-byte header followed by a 373-byte body of zero-padded fixed-width fields.
//
//	[0]      token type (0 = web session token)
//	[1..129] session token
//	[130..194] client IP
//	[195..323] client username
//	[324..372] client MAC (aa-bb-cc-...)
func buildValidatePacket(token, clientIP, username, mac string) []byte {
	const (
		tokenField = 130 // type byte + token
		ipField    = 65
		userField  = 129
		macField   = 49
	)
	h := header{Type: opValidateVideo, Size: VideoPacketSize}
	buf := make([]byte, HeaderSize+VideoPacketSize)
	copy(buf, h.marshal())

	body := buf[HeaderSize:]
	off := 0
	body[off] = 0 // WEB_SESSION_TOKEN
	putFixed(body[off+1:off+tokenField], token)
	off += tokenField
	putFixed(body[off:off+ipField], clientIP)
	off += ipField
	putFixed(body[off:off+userField], username)
	off += userField
	putFixed(body[off:off+macField], mac)
	return buf
}

// localAddrInfo returns the local IP and MAC (dash-separated) for the connection,
// used to populate the validate packet. Best-effort; blanks on failure.
func localAddrInfo(conn net.Conn) (ip, mac string) {
	if ta, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		ip = ta.IP.String()
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagLoopback != 0 || ifc.HardwareAddr == nil {
				continue
			}
			if addrs, _ := ifc.Addrs(); len(addrs) > 0 {
				mac = strings.ReplaceAll(ifc.HardwareAddr.String(), ":", "-")
				break
			}
		}
	}
	return ip, mac
}
