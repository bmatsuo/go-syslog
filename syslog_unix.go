// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !windows,!plan9

package syslog

import (
	"errors"
	"net"
)

// UnixConn opens a connection to the syslog daemon running on the
// local machine using a Unix domain socket.
func UnixConn() (conn net.Conn, err error) {
	logTypes := []string{"unixgram", "unix"}
	logPaths := []string{"/dev/log", "/var/run/syslog"}
	for _, network := range logTypes {
		for _, path := range logPaths {
			conn, err := net.Dial(network, path)
			if err != nil {
				continue
			} else {
				return conn, nil
			}
		}
	}
	return nil, errors.New("Unix syslog delivery error")
}
