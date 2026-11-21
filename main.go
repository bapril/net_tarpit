// Standalone HTTP tarpit: answers with a plausible payload whose Content-Length matches it,
// then keeps streaming random bytes at a fixed bit rate until the client goes away.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

var generators = map[string]func() []byte{
	"text/html": func() []byte {
		return []byte("<!DOCTYPE html><html><head><title>Welcome</title></head><body><h1>Welcome</h1><p>Loading your content...</p></body></html>\n")
	},
	"application/json": func() []byte { return []byte(`{"status":"ok","version":"1.4.2","items":[],"next":null}` + "\n") },
	"application/xml": func() []byte {
		return []byte(`<?xml version="1.0" encoding="UTF-8"?><response><status>ok</status><items/></response>` + "\n")
	},
	"text/xml": func() []byte {
		return []byte(`<?xml version="1.0" encoding="UTF-8"?><response><status>ok</status><items/></response>` + "\n")
	},
	"text/csv":        func() []byte { return []byte("id,name,created\n1,alpha,2024-01-02\n2,beta,2024-01-03\n") },
	"text/plain":      func() []byte { return []byte("OK\n") },
	"image/png":       genPNG,
	"application/pdf": genPDF,
	"application/zip": genZip,
}

var extTypes = map[string]string{
	".html": "text/html", ".htm": "text/html", ".json": "application/json", ".xml": "application/xml",
	".csv": "text/csv", ".txt": "text/plain", ".png": "image/png", ".pdf": "application/pdf", ".zip": "application/zip",
}

func genPNG() []byte {
	img := image.NewGray(image.Rect(0, 0, 64, 64))
	rand.Read(img.Pix)
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func genPDF() []byte {
	// shortcut: no xref table, viewers repair it; upgrade if a strict parser must accept it.
	return []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
		"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")
}

func genZip() []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.Create("readme.txt")
	f.Write([]byte("backup archive\n"))
	w.Close()
	return b.Bytes()
}

// pickType: URL extension first (browsers always list text/html in Accept), then Accept, then HTML.
func pickType(r *http.Request) string {
	if t, ok := extTypes[strings.ToLower(path.Ext(r.URL.Path))]; ok {
		return t
	}
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		t := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if _, ok := generators[t]; ok {
			return t
		}
	}
	return "text/html"
}

// throttle copies r to w at about bps bits/second until r ends or a write fails.
func throttle(w io.Writer, r io.Reader, bps int) error {
	const tick = 50 * time.Millisecond
	buf := make([]byte, max(1, bps/8*int(tick)/int(time.Second)))
	t := time.NewTicker(tick)
	defer t.Stop()
	for range t.C {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
	return nil
}

// milestoneWriter logs when the total sent reaches 1, 2, 4, 8... MiB.
type milestoneWriter struct {
	net.Conn
	id   string
	sent int64
	next int64 // next threshold in MiB
}

func (m *milestoneWriter) Write(p []byte) (int, error) {
	n, err := m.Conn.Write(p)
	m.sent += int64(n)
	for m.sent >= m.next<<20 {
		log.Printf("[%s] %s sent %d MiB", m.id, m.RemoteAddr(), m.next)
		m.next *= 2
	}
	return n, err
}

// asciiRuns yields run copies of each printable ASCII char (0x20-0x7E) in turn, forever.
type asciiRuns struct{ run, i int }

func (a *asciiRuns) Read(p []byte) (int, error) {
	for k := range p {
		p[k] = byte(0x20 + (a.i/a.run)%95)
		a.i++
	}
	return len(p), nil
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func serve(conn net.Conn, bps, run int) {
	defer conn.Close()
	id := newID()
	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		log.Printf("[%s] %s bad request: %v", id, conn.RemoteAddr(), err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	ct := pickType(req)
	body := generators[ct]()
	log.Printf("[%s] %s request %s %s -> %s payload=%dB", id, conn.RemoteAddr(), req.Method, req.URL.Path, ct, len(body))
	hdr := "HTTP/1.1 200 OK\r\nServer: nginx\r\nContent-Type: " + ct +
		"\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n"
	w := &milestoneWriter{Conn: conn, id: id, next: 1}
	err = throttle(w, io.MultiReader(strings.NewReader(hdr), bytes.NewReader(body), &asciiRuns{run: run}), bps)
	log.Printf("[%s] %s session ended after %s, %d bytes sent: %v", id, conn.RemoteAddr(), time.Since(start).Round(time.Millisecond), w.sent, err)
}

func main() {
	def := 2000
	if v, err := strconv.Atoi(os.Getenv("TARPIT_RATE")); err == nil {
		def = v
	}
	addr := flag.String("listen", ":8080", "listen address")
	rate := flag.Int("rate", def, "send rate in bits per second (env TARPIT_RATE)")
	run := flag.Int("run", 10000, "after the payload, send this many of each printable ASCII char in turn")
	flag.Parse()
	if *run < 1 {
		log.Fatal("run must be at least 1")
	}
	if *rate < 8 {
		log.Fatal("rate must be at least 8 bps")
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("tarpit on %s at %d bps\n", *addr, *rate)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c, *rate, *run)
	}
}
