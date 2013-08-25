// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !windows,!plan9

// Package syslog provides a simple interface to the system log
// service. It can send messages to the syslog daemon using UNIX
// domain sockets, UDP or TCP.
//
// Only one call to Dial is necessary. On write failures,
// the syslog client will attempt to reconnect to the server
// and write again.
package syslog

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The Priority is a combination of the syslog facility and
// severity. For example, LOG_ALERT | LOG_FTP sends an alert severity
// message from the FTP facility. The default severity is LOG_EMERG;
// the default facility is LOG_KERN.
type Priority int

const severityMask = 0x07
const facilityMask = 0xf8

const (
	// Severity.

	// From /usr/include/sys/syslog.h.
	// These are the same on Linux, BSD, and OS X.
	LOG_EMERG Priority = iota
	LOG_ALERT
	LOG_CRIT
	LOG_ERR
	LOG_WARNING
	LOG_NOTICE
	LOG_INFO
	LOG_DEBUG
)

const (
	// Facility.

	// From /usr/include/sys/syslog.h.
	// These are the same up to LOG_FTP on Linux, BSD, and OS X.
	LOG_KERN Priority = iota << 3
	LOG_USER
	LOG_MAIL
	LOG_DAEMON
	LOG_AUTH
	LOG_SYSLOG
	LOG_LPR
	LOG_NEWS
	LOG_UUCP
	LOG_CRON
	LOG_AUTHPRIV
	LOG_FTP
	_ // unused
	_ // unused
	_ // unused
	_ // unused
	LOG_LOCAL0
	LOG_LOCAL1
	LOG_LOCAL2
	LOG_LOCAL3
	LOG_LOCAL4
	LOG_LOCAL5
	LOG_LOCAL6
	LOG_LOCAL7
)

var (
	AppendBare Appender = AppenderFunc(
		func(w io.Writer, p Priority, host, tag, msg string) (int, error) {
			return fmt.Fprintf(w, "<%d>%s[%d]: %s%s",
				p, tag, os.Getpid(), msg, termCap(msg))
		},
	)
	AppendStd Appender = AppenderFunc(
		func(w io.Writer, pri Priority, host, tag, msg string) (int, error) {
			timestamp := time.Now().Format(time.Stamp)
			return fmt.Fprintf(w, "<%d>%s %s[%d]: %s%s",
				pri, timestamp, tag, os.Getpid(), msg, termCap(msg))
		},
	)
	AppendRFC3339 Appender = AppenderFunc(
		func(w io.Writer, p Priority, host, tag, msg string) (int, error) {
			timestamp := time.Now().Format(time.RFC3339)
			return fmt.Fprintf(w, "<%d>%s %s %s[%d]: %s%s",
				p, timestamp, host, tag, os.Getpid(), msg, termCap(msg))
		},
	)
)

type Appender interface {
	Append(w io.Writer, p Priority, host, tag, msg string) (int, error)
}

type AppenderFunc func(io.Writer, Priority, string, string, string) (int, error)

func (fn AppenderFunc) Append(w io.Writer, p Priority, host, tag, msg string) (int, error) {
	return fn(w, p, host, tag, msg)
}

func termCap(msg string) string {
	if strings.HasSuffix(msg, "\n") {
		return ""
	} else {
		return "\n"
	}
}

// A Writer is a connection to a syslog server.
type Writer struct {
	priority Priority
	tag      string
	hostname string
	network  string
	raddr    string
	appender Appender

	mu   sync.Mutex // guards conn
	conn net.Conn
}

// New establishes a new connection to the system log daemon.  Each
// write to the returned writer sends a log message with the given
// priority and prefix.
func New(priority Priority, tag string) (w *Writer, err error) {
	return Dial("", "", priority, tag)
}

func NewAppended(pri Priority, tag string, a Appender) (w *Writer, err error) {
	return DialAppended("", "", pri, tag, a)
}

type message struct {
	pri     Priority
	tag     string
	content string
	next    *message
}

type Syslog struct {
	conn      net.Conn
	hostname  string
	facility  Priority
	level     map[string]Priority
	baseLevel Priority
	baseTag   string
	tagSep    string
	appender  Appender
	highwater int64
	in        chan message
	termlock  chan chan chan error
}

type Logger struct {
	syslog *Syslog
	tag    string
	pri    Priority
}

func NewSyslog(conn net.Conn, appender Appender, facility Priority, level Priority, tag string) (*Syslog, error) {
	if conn == nil {
		return nil, fmt.Errorf("nil net.Conn")
	}
	if appender == nil {
		return nil, fmt.Errorf("nil Appender")
	}

	syslog := new(Syslog)
	syslog.conn = conn
	syslog.appender = appender
	syslog.facility = facility
	syslog.baseLevel = level
	syslog.baseTag = tag
	syslog.tagSep = "."

	syslog.hostname, _ = os.Hostname()
	if syslog.hostname == "" {
		syslog.hostname = "localhost"
	}

	syslog.in = make(chan message, 0)
	syslog.termlock = make(chan chan chan error, 1)
	syslog.termlock <- nil

	err := syslog.start()
	if err != nil {
		return nil, err
	}

	return syslog, nil
}

// not threadsafe. the caller must be theradsafe
type messageQueue struct {
	length     int
	head, tail *message
}

var errEmpty = fmt.Errorf("empty")
var errHighwater = fmt.Errorf("highwater")

func (q *messageQueue) Dequeue() (*message, error) {
	if q.length == 0 {
		return nil, errEmpty
	}
	msg := q.head
	q.head = q.head.next
	return msg, nil
}

func (q *messageQueue) Enqueue(highwater int, msg *message) error {
	// check msg first
	if msg == nil {
		return fmt.Errorf("nil msg")
	}
	if msg.next != nil {
		return fmt.Errorf("msg already enqueued")
	}

	if highwater <= q.length {
		return errHighwater
	}

	if q.head == nil {
		q.head = msg
	}
	if q.tail != nil {
		q.tail.next = msg
	}
	q.tail = msg

	return nil
}

func (syslog *Syslog) start() error {
	term := <-syslog.termlock
	if term != nil {
		return fmt.Errorf("already started")
	}

	_connDone := make(chan error, 1)
	connDone := _connDone

	term = make(chan chan error, 1)
	syslog.termlock <- term

	q := new(messageQueue)

	go func() {
		var closed bool
		for cont := true; cont; {
			select {
			case err := <-connDone:
				if err != nil {
					syslog.failure(err)
				}
				if closed {
					cont = false
				} else {
					// TODO configurable dequeue size; grouped errors?
					msg, err := q.Dequeue()
					switch err {
					case errEmpty:
						connDone = nil
					case nil:
						go func() { connDone <- syslog.writeString(msg.pri, msg.tag, msg.content) }()
					default:
						go syslog.failure(err)
					}
				}
			case msg := <-syslog.in:
				highwater := atomic.LoadInt64(&syslog.highwater)
				err := q.Enqueue(int(highwater), &msg)
				if err != nil {
					syslog.drop(&msg, err)
				}
			case errch := <-term:
				if connDone == nil {
					cont = false
				} else {
					closed = true
				}
				errch <- nil
			}
		}
	}()

	return nil
}

func (syslog *Syslog) drop(msg *message, err error) {
	// FIXME fail fast channel send or something
}

func (syslog *Syslog) failure(err error) {
	// FIXME fail fast channel send or something
}

func (syslog *Syslog) Close() error {
	return nil
}

func (syslog *Syslog) Highwater(n int) {
	atomic.StoreInt64(&syslog.highwater, int64(n))
}

func (syslog *Syslog) writeString(level Priority, tag string, msg string) error {
	pri := syslog.facility
	pri = pri | (level & severityMask)
	return syslog.doConn(func(conn net.Conn) error {
		if conn != nil {
			_, err := syslog.appender.Append(conn, pri, syslog.hostname, tag, msg)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (syslog *Syslog) doConn(fn func(net.Conn) error) error {
	return fn(syslog.conn)
}

func (syslog *Syslog) Logger(tag string) *Logger {
	if tag == "" {
		tag = syslog.baseTag
	} else {
		tag = fmt.Sprintf("%s.%s", syslog.baseTag, tag)
	}

	pri := syslog.facility
	level, ok := syslog.level[tag]
	if !ok {
		level = syslog.baseLevel
		if level == 0 {
			level = LOG_INFO
		}
	}
	pri = pri | level

	return &Logger{
		syslog: syslog,
		pri:    pri,
		tag:    tag,
	}
}

func (logger *Logger) Emergf(format string, v ...interface{}) error {
	return logger.Emerg(fmt.Sprintf(format, v...))
}
func (logger *Logger) Alertf(format string, v ...interface{}) error {
	return logger.Alert(fmt.Sprintf(format, v...))
}
func (logger *Logger) Critf(format string, v ...interface{}) error {
	return logger.Crit(fmt.Sprintf(format, v...))
}
func (logger *Logger) Errf(format string, v ...interface{}) error {
	return logger.Err(fmt.Sprintf(format, v...))
}
func (logger *Logger) Warningf(format string, v ...interface{}) error {
	return logger.Warning(fmt.Sprintf(format, v...))
}
func (logger *Logger) Noticef(format string, v ...interface{}) error {
	return logger.Notice(fmt.Sprintf(format, v...))
}
func (logger *Logger) Infof(format string, v ...interface{}) error {
	return logger.Info(fmt.Sprintf(format, v...))
}
func (logger *Logger) Debugf(format string, v ...interface{}) error {
	return logger.Debug(fmt.Sprintf(format, v...))
}
func (logger *Logger) Printf(format string, v ...interface{}) error {
	return logger.Print(fmt.Sprintf(format, v...))
}
func (logger *Logger) PrintLevelf(level Priority, format string, v ...interface{}) error {
	return logger.PrintLevel(level, fmt.Sprintf(format, v...))
}

func (logger *Logger) Emerg(v ...interface{}) error {
	return logger.PrintLevel(LOG_EMERG, v...)
}
func (logger *Logger) Alert(v ...interface{}) error {
	return logger.PrintLevel(LOG_ALERT, v...)
}
func (logger *Logger) Crit(v ...interface{}) error {
	return logger.PrintLevel(LOG_CRIT, v...)
}
func (logger *Logger) Err(v ...interface{}) error {
	return logger.PrintLevel(LOG_ERR, v...)
}
func (logger *Logger) Warning(v ...interface{}) error {
	return logger.PrintLevel(LOG_WARNING, v...)
}
func (logger *Logger) Notice(v ...interface{}) error {
	return logger.PrintLevel(LOG_NOTICE, v...)
}
func (logger *Logger) Info(v ...interface{}) error {
	return logger.PrintLevel(LOG_INFO, v...)
}
func (logger *Logger) Debug(v ...interface{}) error {
	return logger.PrintLevel(LOG_DEBUG, v...)
}
func (logger *Logger) Print(v ...interface{}) error {
	return logger.PrintLevel(logger.pri, v...)
}

func (logger *Logger) PrintLevel(level Priority, v ...interface{}) error {
	return logger.syslog.writeString(level, logger.tag, fmt.Sprint(v...))
}

func (logger *Logger) Write(p []byte) (int, error) {
	err := logger.syslog.writeString(logger.pri, logger.tag, string(p))
	if err != nil {
		return 0, err
	}
	return len(p), err
}

// Dial establishes a connection to a log daemon by connecting to
// address raddr on the network net.  Each write to the returned
// writer sends a log message with the given facility, severity and
// tag.
func Dial(network, raddr string, priority Priority, tag string) (*Writer, error) {
	if priority < 0 || priority > LOG_LOCAL7|LOG_DEBUG {
		return nil, errors.New("log/syslog: invalid priority")
	}

	if tag == "" {
		tag = os.Args[0]
	}
	hostname, _ := os.Hostname()

	w := &Writer{
		priority: priority,
		tag:      tag,
		hostname: hostname,
		network:  network,
		raddr:    raddr,
		appender: AppendRFC3339,
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	err := w.connect()
	if err != nil {
		return nil, err
	}
	return w, err
}

func DialAppended(net, raddr string, pri Priority, tag string, a Appender) (*Writer, error) {
	if a == nil {
		return nil, fmt.Errorf("nil Appender")
	}
	w, err := Dial(net, raddr, pri, tag)
	if err != nil {
		return nil, err
	}
	w.appender = a
	return w, nil
}

// connect makes a connection to the syslog server.
// It must be called with w.mu held.
func (w *Writer) connect() (err error) {
	if w.conn != nil {
		// ignore err from close, it makes sense to continue anyway
		w.conn.Close()
		w.conn = nil
	}

	if w.network == "" {
		w.conn, err = UnixConn()
		if w.hostname == "" {
			w.hostname = "localhost"
		}
	} else {
		var c net.Conn
		c, err = net.Dial(w.network, w.raddr)
		if err == nil {
			w.conn = c
			if w.hostname == "" {
				w.hostname = c.LocalAddr().String()
			}
		}
	}
	return
}

// Write sends a log message to the syslog daemon.
func (w *Writer) Write(b []byte) (int, error) {
	return w.writeAndRetry(w.priority, string(b))
}

// Close closes a connection to the syslog daemon.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		err := w.conn.Close()
		w.conn = nil
		return err
	}
	return nil
}

// Emerg logs a message with severity LOG_EMERG, ignoring the severity
// passed to New.
func (w *Writer) Emerg(m string) (err error) {
	_, err = w.writeAndRetry(LOG_EMERG, m)
	return err
}

// Alert logs a message with severity LOG_ALERT, ignoring the severity
// passed to New.
func (w *Writer) Alert(m string) (err error) {
	_, err = w.writeAndRetry(LOG_ALERT, m)
	return err
}

// Crit logs a message with severity LOG_CRIT, ignoring the severity
// passed to New.
func (w *Writer) Crit(m string) (err error) {
	_, err = w.writeAndRetry(LOG_CRIT, m)
	return err
}

// Err logs a message with severity LOG_ERR, ignoring the severity
// passed to New.
func (w *Writer) Err(m string) (err error) {
	_, err = w.writeAndRetry(LOG_ERR, m)
	return err
}

// Wanring logs a message with severity LOG_WARNING, ignoring the
// severity passed to New.
func (w *Writer) Warning(m string) (err error) {
	_, err = w.writeAndRetry(LOG_WARNING, m)
	return err
}

// Notice logs a message with severity LOG_NOTICE, ignoring the
// severity passed to New.
func (w *Writer) Notice(m string) (err error) {
	_, err = w.writeAndRetry(LOG_NOTICE, m)
	return err
}

// Info logs a message with severity LOG_INFO, ignoring the severity
// passed to New.
func (w *Writer) Info(m string) (err error) {
	_, err = w.writeAndRetry(LOG_INFO, m)
	return err
}

// Debug logs a message with severity LOG_DEBUG, ignoring the severity
// passed to New.
func (w *Writer) Debug(m string) (err error) {
	_, err = w.writeAndRetry(LOG_DEBUG, m)
	return err
}

func (w *Writer) writeAndRetry(p Priority, s string) (int, error) {
	pr := (w.priority & facilityMask) | (p & severityMask)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		if n, err := w.write(pr, s); err == nil {
			return n, err
		}
	}
	if err := w.connect(); err != nil {
		return 0, err
	}
	return w.write(pr, s)
}

// write generates and writes a syslog formatted string. The
// format is as follows: <PRI>TIMESTAMP HOSTNAME TAG[PID]: MSG
func (w *Writer) write(p Priority, msg string) (int, error) {
	return w.appender.Append(w.conn, p, w.hostname, w.tag, msg)
}

// NewLogger creates a log.Logger whose output is written to
// the system log service with the specified priority. The logFlag
// argument is the flag set passed through to log.New to create
// the Logger.
func NewLogger(p Priority, logFlag int) (*log.Logger, error) {
	s, err := New(p, "")
	if err != nil {
		return nil, err
	}
	return log.New(s, "", logFlag), nil
}

func NewLoggerAppended(pri Priority, logFlag int, a Appender) (*log.Logger, error) {
	s, err := NewAppended(pri, "", a)
	if err != nil {
		return nil, err
	}
	return log.New(s, "", logFlag), nil
}
