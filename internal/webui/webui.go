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
	"time"

	"github.com/coder/websocket"

	"rd450x-console/internal/rfb"
)

//go:embed all:novnc
var novncFiles embed.FS

// Serve starts the web server on listen, serving noVNC and a /websockify RFB
// endpoint backed by src/sink. It blocks until ctx is cancelled.
func Serve(ctx context.Context, listen string, src rfb.Source, sink rfb.Sink, openBrowser bool) error {
	sub, err := fs.Sub(novncFiles, "novnc")
	if err != nil {
		return err
	}

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

		connCtx := r.Context()
		nc := websocket.NetConn(connCtx, c, websocket.MessageBinary)
		log.Printf("webui: noVNC connected from %s", r.RemoteAddr)
		if err := rfb.Serve(connCtx, nc, src, sink); err != nil {
			log.Printf("webui: rfb session ended: %v", err)
		}
	})

	srv := &http.Server{Addr: listen, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	url := fmt.Sprintf("http://%s/vnc.html?autoconnect=true&path=websockify&resize=scale", listen)
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
