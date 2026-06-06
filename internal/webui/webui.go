// Package webui serves the embedded noVNC client and bridges its WebSocket to
// the in-process RFB server.
package webui

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"

	"rd450x-console/internal/rfb"
)

// shutdownGrace is how long the bridge waits after the last noVNC session ends
// before tearing itself down. It lets a page reload (a disconnect immediately
// followed by a reconnect) keep the bridge — and the BMC session — alive.
const shutdownGrace = 5 * time.Second

//go:embed all:novnc
var novncFiles embed.FS

// Serve starts the web server on listen, serving noVNC and a /websockify RFB
// endpoint backed by src/sink. It blocks until ctx is cancelled.
//
// onDisconnect, if non-nil, is invoked once a connected noVNC session ends
// (the browser tab is closed or navigates away). The caller uses it to tear the
// whole bridge down — cancelling ctx so the BMC client closes and releases its
// web session — instead of leaving an orphaned video/web session on the card.
func Serve(ctx context.Context, listen string, src rfb.Source, sink rfb.Sink, openBrowser bool, onDisconnect func()) error {
	sub, err := fs.Sub(novncFiles, "novnc")
	if err != nil {
		return err
	}

	// Track live noVNC sessions so a closed tab (no reconnect within the grace
	// period) tears the bridge down, while a page reload does not.
	var (
		connMu     sync.Mutex
		activeConn int
		graceTimer *time.Timer
	)

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/websockify", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // localhost only
			Subprotocols:       []string{"binary"},
		})
		if err != nil {
			log.Printf("webui: ws accept: %v", err)
			return
		}
		defer c.CloseNow()

		// Session starting: cancel any pending idle-shutdown.
		connMu.Lock()
		activeConn++
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer = nil
		}
		connMu.Unlock()

		connCtx := r.Context()
		nc := websocket.NetConn(connCtx, c, websocket.MessageBinary)
		log.Printf("webui: noVNC connected from %s", r.RemoteAddr)
		if err := rfb.Serve(connCtx, nc, src, sink); err != nil {
			log.Printf("webui: rfb session ended: %v", err)
		}

		// Session ended. If nothing reconnects within the grace period (i.e. the
		// tab was closed, not reloaded), tear the bridge down so we don't leak
		// the BMC video/web session (the card's session pool is tiny).
		if onDisconnect == nil {
			return
		}
		connMu.Lock()
		activeConn--
		if activeConn == 0 {
			graceTimer = time.AfterFunc(shutdownGrace, func() {
				connMu.Lock()
				idle := activeConn == 0
				connMu.Unlock()
				if idle {
					log.Printf("webui: noVNC idle for %s — shutting down bridge and releasing BMC session", shutdownGrace)
					onDisconnect()
				}
			})
		}
		connMu.Unlock()
	})

	srv := &http.Server{Addr: listen, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// resize=off renders the framebuffer 1:1 (no scaling), matching JViewer's
	// pixel-perfect output. Scaling to fit (resize=scale) blurs with bilinear or,
	// with nearest-neighbour, unevenly thickens pixel-font strokes at the
	// fractional zoom factor — so we keep it off.
	url := fmt.Sprintf("http://%s/vnc.html?autoconnect=true&path=websockify&resize=off", listen)
	log.Printf("webui: open %s", url)
	if openBrowser {
		go openURL(url)
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("webui: could not open browser: %v", err)
	}
}
