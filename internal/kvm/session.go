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
	reArg    = regexp.MustCompile(`<argument>([^<]*)</argument>`)
)

// Login authenticates to the MegaRAC web UI and obtains a KVM video-session token:
//
//  1. POST /rpc/WEBSES/create.asp  → SESSION_COOKIE
//  2. GET  /Java/jviewer.jnlp?EXTRNIP=<host>&JNLPSTR=JViewer (with the cookie)
//
// Fetching the launch jnlp is the step that ALLOCATES the video session on the
// card and embeds a fresh -kvmtoken bound to it. Minting a bare token via
// getsessiontoken.asp instead yields a token with no video-session info, which
// the card rejects at validate time with status 3 (INVALID_VIDEO_SESSION_INFO).
//
// The password is sent over the BMC HTTP session only and is never logged.
func Login(ctx context.Context, host, user, password string) (WebSession, error) {
	args, cookie, err := FetchLaunchArgs(ctx, host, user, password)
	if err != nil {
		return WebSession{}, err
	}
	token := args["kvmtoken"]
	if token == "" {
		return WebSession{}, fmt.Errorf("launch jnlp: no -kvmtoken in response (parsed %d args)", len(args))
	}
	webcookie := args["webcookie"]
	if webcookie == "" {
		webcookie = cookie
	}
	return WebSession{Token: token, Cookie: webcookie}, nil
}

// FetchLaunchArgs performs the two-step web login and returns the full parsed
// JViewer jnlp argument map plus the create.asp session cookie. Beyond the
// kvmtoken/webcookie that Login extracts, the map carries the virtual-media
// parameters the vmedia data plane needs: kvmport, cdport/fdport/hdport,
// cdnum/fdnum/hdnum, singleportenabled, vmsecure/kvmsecure.
//
// The token/cookie values in the map are secrets — callers must not log them.
func FetchLaunchArgs(ctx context.Context, host, user, password string) (map[string]string, string, error) {
	hc := newWebClient(15 * time.Second)
	var errs []string
	// Retain the RD450X's original port-80 flow, then fall back to HTTPS for
	// MegaRAC variants that expose the launcher only on their secure listener.
	for _, base := range []string{"http://" + host, "https://" + host} {
		args, cookie, err := fetchLaunchArgsAtBase(ctx, hc, base, host, user, password)
		if err == nil {
			return args, cookie, nil
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		if cookie != "" {
			logoutAtBase(hc, base, cookie)
		}
		errs = append(errs, fmt.Sprintf("%s: %v", strings.TrimSuffix(base, host), err))
	}
	return nil, "", fmt.Errorf("BMC web launch failed (%s)", strings.Join(errs, "; "))
}

func fetchLaunchArgsAtBase(ctx context.Context, hc *http.Client, base, host, user, password string) (map[string]string, string, error) {
	// Step 1: create web session.
	form := url.Values{"WEBVAR_USERNAME": {user}, "WEBVAR_PASSWORD": {password}}
	body, err := httpDo(ctx, hc, http.MethodPost, base+"/rpc/WEBSES/create.asp",
		strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return nil, "", fmt.Errorf("web login: %w", err)
	}
	m := reCookie.FindStringSubmatch(body)
	if m == nil {
		return nil, "", fmt.Errorf("web login: no SESSION_COOKIE in response (bad credentials?)")
	}
	cookie := m[1]

	// Step 2: fetch the launch jnlp to allocate the video session.
	jnlpURL := base + "/Java/jviewer.jnlp?EXTRNIP=" + url.QueryEscape(host) + "&JNLPSTR=JViewer"
	body, err = httpDo(ctx, hc, http.MethodGet, jnlpURL, nil,
		map[string]string{"Cookie": "SessionCookie=" + cookie})
	if err != nil {
		return nil, cookie, fmt.Errorf("launch jnlp: %w", err)
	}
	if strings.Contains(body, "session_expired") {
		return nil, cookie, fmt.Errorf("launch jnlp: BMC rejected the web session (session_expired) — " +
			"likely too many stale web sessions on the card; wait for them to idle out or reduce concurrent logins")
	}
	return parseJNLPArgs(body), cookie, nil
}

// Logout best-effort releases a BMC web session so it does not linger and
// exhaust the card's small session pool. Errors are ignored.
func Logout(host, cookie string) {
	if cookie == "" {
		return
	}
	hc := newWebClient(8 * time.Second)
	for _, base := range []string{"http://" + host, "https://" + host} {
		if logoutAtBase(hc, base, cookie) {
			return
		}
	}
}

func logoutAtBase(hc *http.Client, base, cookie string) bool {
	req, err := http.NewRequest(http.MethodGet, base+"/rpc/WEBSES/logout.asp", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Cookie", "SessionCookie="+cookie)
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	return false
}

// newWebClient accepts the old, self-signed certificate presented by this
// generation of MegaRAC. Callers should reach it only over a trusted management
// path because this deliberately does not authenticate the peer certificate.
func newWebClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:   legacyTLSConfig(),
			DisableKeepAlives: true,
		},
	}
}

// parseJNLPArgs extracts the <argument>-name</argument><argument>value</argument>
// pairs from a JViewer jnlp into a map keyed by the flag name (leading '-' stripped).
func parseJNLPArgs(body string) map[string]string {
	ms := reArg.FindAllStringSubmatch(body, -1)
	out := make(map[string]string, len(ms)/2)
	for i := 0; i+1 < len(ms); i += 2 {
		name := strings.TrimPrefix(strings.TrimSpace(ms[i][1]), "-")
		out[name] = strings.TrimSpace(ms[i+1][1])
	}
	return out
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

// buildLegacyValidatePacket assembles the exact IVTP type=18 packet produced by
// the JViewer bundled with the ASUS ESC8000 G3. The packet is 332 bytes total,
// and its Size header unusually contains that total rather than the 324-byte
// body length.
//
//	[0]      token type (0 = web session token)
//	[1..129] session token
//	[130..194] client IP
//	[195..323] client username
func buildLegacyValidatePacket(token, clientIP, username string) []byte {
	const (
		tokenField = 130 // type byte + token
		ipField    = 65
		userField  = 129
	)
	h := header{Type: opValidateVideo, Size: LegacyVideoPacketSize}
	buf := make([]byte, LegacyVideoPacketSize)
	copy(buf, h.marshal())

	body := buf[HeaderSize:]
	off := 0
	body[off] = 0 // WEB_SESSION_TOKEN
	putFixed(body[off+1:off+tokenField], token)
	off += tokenField
	putFixed(body[off:off+ipField], clientIP)
	off += ipField
	putFixed(body[off:off+userField], username)
	return buf
}

// buildValidatePacket builds the newer dedicated-port validation packet used
// by the original RD450X implementation.
func buildValidatePacket(token, clientIP, username, mac string) []byte {
	const (
		tokenField = 130
		ipField    = 65
		userField  = 129
		macField   = 49
	)
	h := header{Type: opValidateVideo, Size: ModernVideoPacketBodySize}
	buf := make([]byte, HeaderSize+ModernVideoPacketBodySize)
	copy(buf, h.marshal())

	body := buf[HeaderSize:]
	body[0] = 0
	putFixed(body[1:tokenField], token)
	off := tokenField
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
