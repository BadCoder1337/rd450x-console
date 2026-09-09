// Command vmedia_ws_probe_go mounts a local image through rd450x-console's
// browser-backed /control protocol. It is a diagnostic stand-in for the noVNC
// File.slice client and exercises the same request/response path end-to-end.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/coder/websocket"
)

type attach struct {
	Type string `json:"type"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type status struct {
	Type  string `json:"type"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Error string `json:"error"`
}

func main() {
	image := flag.String("image", "", "local ISO/disk image to serve")
	kind := flag.String("kind", "cd", "virtual device kind: cd, fd, or hd")
	url := flag.String("url", "ws://127.0.0.1:6080/control", "control WebSocket URL")
	flag.Parse()
	if *image == "" {
		log.Fatal("-image is required")
	}

	f, err := os.Open(*image)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		log.Fatal(err)
	}
	dev, ok := map[string]byte{"cd": 0, "fd": 1, "hd": 2}[*kind]
	if !ok {
		log.Fatalf("invalid -kind %q", *kind)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c, _, err := websocket.Dial(ctx, *url, &websocket.DialOptions{HTTPClient: http.DefaultClient})
	if err != nil {
		log.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	b, _ := json.Marshal(attach{Type: "vmedia.attach", Kind: *kind, Name: filepath.Base(*image), Size: fi.Size()})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		log.Fatal(err)
	}
	log.Printf("attach requested: %s (%d bytes)", filepath.Base(*image), fi.Size())

	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Fatal(err)
		}
		if typ == websocket.MessageText {
			var s status
			if json.Unmarshal(msg, &s) == nil && s.Type == "vmedia.status" {
				log.Printf("%s: %s %s", s.Kind, s.State, s.Error)
			}
			continue
		}
		if len(msg) < 18 {
			continue
		}
		op := msg[0]
		requestDev := msg[1]
		id := binary.BigEndian.Uint32(msg[2:6])
		off := binary.BigEndian.Uint64(msg[6:14])
		n := binary.BigEndian.Uint32(msg[14:18])
		if op != 0 || requestDev != dev {
			writeResponse(ctx, c, id, 1, nil)
			continue
		}
		data := make([]byte, n)
		got, readErr := f.ReadAt(data, int64(off))
		if readErr != nil && readErr != io.EOF {
			log.Printf("read off=%d len=%d: %v", off, n, readErr)
			writeResponse(ctx, c, id, 1, nil)
			continue
		}
		log.Printf("read off=%d len=%d -> %d", off, n, got)
		writeResponse(ctx, c, id, 0, data[:got])
	}
}

func writeResponse(ctx context.Context, c *websocket.Conn, id uint32, status byte, data []byte) {
	msg := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(msg[0:4], id)
	msg[4] = status
	copy(msg[5:], data)
	if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
		log.Fatal(fmt.Errorf("write response: %w", err))
	}
}
