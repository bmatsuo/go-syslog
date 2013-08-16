// Copyright 2013, Bryan Matsuo. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// rsyslog.go [created: Fri, 16 Aug 2013]

package rsyslog

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bmatsuo/go-syslog"
)

var (
	// standard rsyslog appender
	Append syslog.Appender = syslog.AppenderFunc(
		func(w io.Writer, pri syslog.Priority, host, tag, msg string) (int, error) {
			timestamp := time.Now().Format(time.Stamp)
			return fmt.Fprintf(w, "<%d>%s %s[%d]: %s%s",
				pri, timestamp, tag, os.Getpid(), msg, termCap(msg))
		},
	)
)

func termCap(msg string) string {
	if strings.HasSuffix(msg, "\n") {
		return ""
	} else {
		return "\n"
	}
}
