// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !windows,!plan9

package syslog

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// not a particularly fair benchmark. everything gets queued up.
// but seriously synchronous logging w/o batching is not very good.
func BenchmarkSyslogType(b *testing.B) {
	conn, err := UnixConn()
	if err != nil {
		b.Fatal(err)
	}
	slog, err := NewSyslog(conn,
		AppendStd,
		LOG_LOCAL0,
		LOG_NOTICE,
		"go-syslog")
	if err != nil {
		b.Fatal(err)
	}
	logger := slog.Logger("")

	for i := 0; i < b.N; i++ {
		logger.Critf("Syslog %d", i)
	}
}

func BenchmarkWriter(b *testing.B) {
	w, err := Dial("", "",
		LOG_LOCAL0|LOG_NOTICE,
		"go-syslog")
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		w.Crit(fmt.Sprintf("Writer %d", i))
	}
}

func BenchmarkWriterAppended(b *testing.B) {
	w, err := DialAppended("", "",
		LOG_LOCAL0|LOG_NOTICE,
		"go-syslog",
		AppendStd)
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		w.Crit(fmt.Sprintf("Writer AppendStd %d", i))
	}
}

func BenchmarkWriterBare(b *testing.B) {
	w, err := DialAppended("", "",
		LOG_LOCAL0|LOG_NOTICE,
		"go-syslog",
		AppendBare)
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		w.Crit(fmt.Sprintf("Writer AppendBare %d", i))
	}
}

// a mock net.Conn implementation
type ConnRecorder struct {
	writes  [][]byte
	writech chan []byte
}

func NewConnRecorder(writech chan<- []byte) *ConnRecorder {
	conn := new(ConnRecorder)
	conn.writech = make(chan []byte)
	go func() {
		for p := range conn.writech {
			conn.writes = append(conn.writes, p)
			select {
			case writech <- p:
				break
			default:
				break
			}
		}
	}()
	return conn
}

func (conn *ConnRecorder) Read([]byte) (int, error) {
	return -1, fmt.Errorf("cannot read")
}

func (conn *ConnRecorder) Write(p []byte) (int, error) {
	conn.writech <- p
	return len(p), nil
}

func (conn *ConnRecorder) Close() error {
	close(conn.writech)
	return nil
}

func (conn *ConnRecorder) LocalAddr() net.Addr {
	return nil
}

func (conn *ConnRecorder) RemoteAddr() net.Addr {
	return nil
}

func (conn *ConnRecorder) SetDeadline(time.Time) error {
	return fmt.Errorf("unimplemented")
}

func (conn *ConnRecorder) SetReadDeadline(time.Time) error {
	return fmt.Errorf("unimplemented")
}

func (conn *ConnRecorder) SetWriteDeadline(time.Time) error {
	return fmt.Errorf("unimplemented")
}

func TestSyslog(t *testing.T) {
	writech := make(chan []byte, 1)
	conn := NewConnRecorder(writech)
	slog, err := NewSyslog(conn, AppendRFC3339, LOG_LOCAL0, LOG_INFO, "go-syslog")
	if err != nil {
		t.Fatal("could not init: ", err)
	}

	logger := slog.Logger("test")
	for i := 0; i < 10; i++ {
		msg := fmt.Sprintf("loop iteration: ", i)
		err := logger.Notice(msg)
		if err != nil {
			t.Fatal(err)
		}

		p := <-writech
		if bytes.Index(p, []byte(msg)) < 0 {
			t.Fatal("message not found")
		}
	}

	slog.Close()
	conn.Close()
}

func runPktSyslog(c net.PacketConn, done chan<- string) {
	var buf [4096]byte
	var rcvd string
	ct := 0
	for {
		var n int
		var err error

		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err = c.ReadFrom(buf[:])
		rcvd += string(buf[:n])
		if err != nil {
			if oe, ok := err.(*net.OpError); ok {
				if ct < 3 && oe.Temporary() {
					ct++
					continue
				}
			}
			break
		}
	}
	c.Close()
	done <- rcvd
}

var crashy = false

func runStreamSyslog(l net.Listener, done chan<- string, wg *sync.WaitGroup) {
	for {
		var c net.Conn
		var err error
		if c, err = l.Accept(); err != nil {
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b := bufio.NewReader(c)
			for ct := 1; !crashy || ct&7 != 0; ct++ {
				s, err := b.ReadString('\n')
				if err != nil {
					break
				}
				done <- s
			}
			c.Close()
		}(c)
	}
}

func startServer(n, la string, done chan<- string) (addr string, sock io.Closer, wg *sync.WaitGroup) {
	if n == "udp" || n == "tcp" {
		la = "127.0.0.1:0"
	} else {
		// unix and unixgram: choose an address if none given
		if la == "" {
			// use ioutil.TempFile to get a name that is unique
			f, err := ioutil.TempFile("", "syslogtest")
			if err != nil {
				log.Fatal("TempFile: ", err)
			}
			f.Close()
			la = f.Name()
		}
		os.Remove(la)
	}

	wg = new(sync.WaitGroup)
	if n == "udp" || n == "unixgram" {
		l, e := net.ListenPacket(n, la)
		if e != nil {
			log.Fatalf("startServer failed: %v", e)
		}
		addr = l.LocalAddr().String()
		sock = l
		wg.Add(1)
		go func() {
			defer wg.Done()
			runPktSyslog(l, done)
		}()
	} else {
		l, e := net.Listen(n, la)
		if e != nil {
			log.Fatalf("startServer failed: %v", e)
		}
		addr = l.Addr().String()
		sock = l
		wg.Add(1)
		go func() {
			defer wg.Done()
			runStreamSyslog(l, done, wg)
		}()
	}
	return
}

func TestWithSimulated(t *testing.T) {
	msg := "Test 123"
	transport := []string{"unix", "unixgram", "udp", "tcp"}

	for _, tr := range transport {
		done := make(chan string)
		addr, _, _ := startServer(tr, "", done)
		if tr == "unix" || tr == "unixgram" {
			defer os.Remove(addr)
		}
		s, err := Dial(tr, addr, LOG_INFO|LOG_USER, "syslog_test")
		if err != nil {
			t.Fatalf("Dial() failed: %v", err)
		}
		err = s.Info(msg)
		if err != nil {
			t.Fatalf("log failed: %v", err)
		}
		check(t, msg, <-done)
		s.Close()
	}
}

func TestFlap(t *testing.T) {
	net := "unix"
	done := make(chan string)
	addr, sock, _ := startServer(net, "", done)
	defer os.Remove(addr)
	defer sock.Close()

	s, err := Dial(net, addr, LOG_INFO|LOG_USER, "syslog_test")
	if err != nil {
		t.Fatalf("Dial() failed: %v", err)
	}
	msg := "Moo 2"
	err = s.Info(msg)
	if err != nil {
		t.Fatalf("log failed: %v", err)
	}
	check(t, msg, <-done)

	// restart the server
	_, sock2, _ := startServer(net, addr, done)
	defer sock2.Close()

	// and try retransmitting
	msg = "Moo 3"
	err = s.Info(msg)
	if err != nil {
		t.Fatalf("log failed: %v", err)
	}
	check(t, msg, <-done)

	s.Close()
}

func TestNew(t *testing.T) {
	if LOG_LOCAL7 != 23<<3 {
		t.Fatalf("LOG_LOCAL7 has wrong value")
	}
	if testing.Short() {
		// Depends on syslog daemon running, and sometimes it's not.
		t.Skip("skipping syslog test during -short")
	}

	s, err := New(LOG_INFO|LOG_USER, "the_tag")
	if err != nil {
		t.Fatalf("New() failed: %s", err)
	}
	// Don't send any messages.
	s.Close()
}

func TestNewLogger(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping syslog test during -short")
	}
	f, err := NewLogger(LOG_USER|LOG_INFO, 0)
	if f == nil {
		t.Error(err)
	}
}

func TestDial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping syslog test during -short")
	}
	f, err := Dial("", "", (LOG_LOCAL7|LOG_DEBUG)+1, "syslog_test")
	if f != nil {
		t.Fatalf("Should have trapped bad priority")
	}
	f, err = Dial("", "", -1, "syslog_test")
	if f != nil {
		t.Fatalf("Should have trapped bad priority")
	}
	l, err := Dial("", "", LOG_USER|LOG_ERR, "syslog_test")
	if err != nil {
		t.Fatalf("Dial() failed: %s", err)
	}
	l.Close()
}

func check(t *testing.T, in, out string) {
	tmpl := fmt.Sprintf("<%d>%%s %%s syslog_test[%%d]: %s\n", LOG_USER+LOG_INFO, in)
	if hostname, err := os.Hostname(); err != nil {
		t.Error("Error retrieving hostname")
	} else {
		var parsedHostname, timestamp string
		var pid int
		if n, err := fmt.Sscanf(out, tmpl, &timestamp, &parsedHostname, &pid); n != 3 || err != nil || hostname != parsedHostname {
			t.Errorf("Got %q, does not match template %q (%d %s)", out, tmpl, n, err)
		}
	}
}

func TestWrite(t *testing.T) {
	tests := []struct {
		pri Priority
		pre string
		msg string
		exp string
	}{
		{LOG_USER | LOG_ERR, "syslog_test", "", "%s %s syslog_test[%d]: \n"},
		{LOG_USER | LOG_ERR, "syslog_test", "write test", "%s %s syslog_test[%d]: write test\n"},
		// Write should not add \n if there already is one
		{LOG_USER | LOG_ERR, "syslog_test", "write test 2\n", "%s %s syslog_test[%d]: write test 2\n"},
	}

	if hostname, err := os.Hostname(); err != nil {
		t.Fatalf("Error retrieving hostname")
	} else {
		for _, test := range tests {
			done := make(chan string)
			addr, sock, _ := startServer("udp", "", done)
			defer sock.Close()
			l, err := Dial("udp", addr, test.pri, test.pre)
			if err != nil {
				t.Fatalf("syslog.Dial() failed: %v", err)
			}
			_, err = io.WriteString(l, test.msg)
			if err != nil {
				t.Fatalf("WriteString() failed: %v", err)
			}
			rcvd := <-done
			test.exp = fmt.Sprintf("<%d>", test.pri) + test.exp
			var parsedHostname, timestamp string
			var pid int
			if n, err := fmt.Sscanf(rcvd, test.exp, &timestamp, &parsedHostname, &pid); n != 3 || err != nil || hostname != parsedHostname {
				t.Errorf("s.Info() = '%q', didn't match '%q' (%d %s)", rcvd, test.exp, n, err)
			}
		}
	}
}

func TestConcurrentWrite(t *testing.T) {
	addr, sock, _ := startServer("udp", "", make(chan string))
	defer sock.Close()
	w, err := Dial("udp", addr, LOG_USER|LOG_ERR, "how's it going?")
	if err != nil {
		t.Fatalf("syslog.Dial() failed: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			err := w.Info("test")
			if err != nil {
				t.Errorf("Info() failed: %v", err)
				return
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func TestConcurrentReconnect(t *testing.T) {
	crashy = true
	defer func() { crashy = false }()

	net := "unix"
	done := make(chan string)
	addr, sock, srvWG := startServer(net, "", done)
	defer os.Remove(addr)

	// count all the messages arriving
	count := make(chan int)
	go func() {
		ct := 0
		for _ = range done {
			ct++
			// we are looking for 500 out of 1000 events
			// here because lots of log messages are lost
			// in buffers (kernel and/or bufio)
			if ct > 500 {
				break
			}
		}
		count <- ct
	}()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			w, err := Dial(net, addr, LOG_USER|LOG_ERR, "tag")
			if err != nil {
				t.Fatalf("syslog.Dial() failed: %v", err)
			}
			for i := 0; i < 100; i++ {
				err := w.Info("test")
				if err != nil {
					t.Errorf("Info() failed: %v", err)
					return
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
	sock.Close()
	srvWG.Wait()
	close(done)

	select {
	case <-count:
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout in concurrent reconnect")
	}
}
