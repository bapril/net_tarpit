package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
)

func TestLengthThenOverrun(t *testing.T) {
	for _, tc := range []struct{ req, want string }{
		{"GET /a.json HTTP/1.1\r\nHost: x\r\n\r\n", "application/json"},
		{"GET / HTTP/1.1\r\nHost: x\r\nAccept: image/png,*/*\r\n\r\n", "image/png"},
	} {
		c, s := net.Pipe()
		go serve(s, 80000, 10000)
		c.Write([]byte(tc.req))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Content-Type") != tc.want {
			t.Fatalf("got %s want %s", resp.Header.Get("Content-Type"), tc.want)
		}
		if n := len(generators[tc.want]()); tc.want == "application/json" && int(resp.ContentLength) != n {
			t.Fatalf("length %d != %d", resp.ContentLength, n)
		}
		if _, err := io.CopyN(io.Discard, br, resp.ContentLength+50); err != nil {
			t.Fatalf("no overrun bytes: %v", err)
		}
		c.Close()
	}
}

func TestExtensionBeatsBrowserAccept(t *testing.T) {
	r, _ := http.NewRequest("GET", "/x.json", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	if got := pickType(r); got != "application/json" {
		t.Fatalf("got %s", got)
	}
}

func TestMilestones(t *testing.T) {
	c, s := net.Pipe()
	go io.Copy(io.Discard, c)
	m := &milestoneWriter{Conn: s, next: 1}
	m.Write(make([]byte, 5<<20))
	if m.next != 8 { // logged 1, 2, 4
		t.Fatalf("next=%d", m.next)
	}
}

func TestAsciiRuns(t *testing.T) {
	a := &asciiRuns{run: 3}
	p := make([]byte, 7)
	a.Read(p)
	if string(p) != "   !!!\"" {
		t.Fatalf("got %q", p)
	}
}

func TestNewID(t *testing.T) {
	id := newID()
	if len(id) != 36 || id[14] != '4' || newID() == id {
		t.Fatalf("bad id %s", id)
	}
}
