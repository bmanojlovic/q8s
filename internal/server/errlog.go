package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// handshakeLogMarker is the prefix net/http uses for every failed TLS
// handshake. When the API port is reachable from the internet these lines are
// pure scanner noise: client certs are required, so the handshake is meant to
// fail for anybody without a certificate.
const handshakeLogMarker = "http: TLS handshake error from"

// handshakeSummaryInterval is the minimum spacing between summary lines.
const handshakeSummaryInterval = 10 * time.Minute

// handshakeFilter is an io.Writer that drops net/http TLS handshake errors and
// forwards every other line unchanged. Dropped lines are counted and reported
// with one summary line at most once per handshakeSummaryInterval. The summary
// is emitted lazily, when the next line arrives after the interval, so the
// filter needs no goroutine or ticker.
//
// It is safe for concurrent use: net/http logs from several goroutines.
type handshakeFilter struct {
	dst io.Writer
	now func() time.Time

	mu          sync.Mutex
	suppressed  int       // handshake lines dropped since the last summary
	lastIP      string    // IP of the most recent dropped handshake line
	lastSummary time.Time // when the last summary was written
}

// newHandshakeFilter returns a filter writing to dst and using now as its
// clock. A nil clock means time.Now.
func newHandshakeFilter(dst io.Writer, now func() time.Time) *handshakeFilter {
	if now == nil {
		now = time.Now
	}
	return &handshakeFilter{
		dst:         dst,
		now:         now,
		lastSummary: now(),
	}
}

// newErrorLogger returns a *log.Logger suitable for http.Server.ErrorLog. Its
// timestamps match the standard logger net/http falls back to when ErrorLog is
// nil.
func newErrorLogger(dst io.Writer, now func() time.Time) *log.Logger {
	return log.New(newHandshakeFilter(dst, now), "", log.LstdFlags)
}

// Write implements io.Writer.
func (f *handshakeFilter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Emit a pending summary first: it describes the interval that just
	// ended, before the arriving line becomes part of the next one.
	f.maybeSummarize()

	total := len(p)
	written := 0
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			line, p = p[:i+1], p[i+1:]
		} else {
			p = nil
		}

		if bytes.Contains(line, []byte(handshakeLogMarker)) {
			f.suppressed++
			if ip := handshakeSourceIP(string(line)); ip != "" {
				f.lastIP = ip
			}
			continue
		}

		n, err := f.dst.Write(line)
		written += n
		if err != nil {
			return written, err
		}
		if n < len(line) {
			return written, io.ErrShortWrite
		}
	}

	// Dropped lines still count as accepted input.
	return total, nil
}

// maybeSummarize writes the pending summary line if the interval has passed
// and anything was suppressed in the meantime. Callers must hold f.mu.
func (f *handshakeFilter) maybeSummarize() {
	now := f.now()
	if f.suppressed == 0 || now.Sub(f.lastSummary) < handshakeSummaryInterval {
		return
	}

	lastIP := f.lastIP
	if lastIP == "" {
		lastIP = "unknown"
	}
	fmt.Fprintf(f.dst, "tls: %d failed handshakes suppressed in the last 10m (last from %s)\n", f.suppressed, lastIP)
	f.suppressed = 0
	f.lastIP = ""
	f.lastSummary = now
}

// handshakeSourceIP extracts the client IP from a net/http handshake error
// line such as
//
//	http: TLS handshake error from 192.0.2.10:51234: EOF
//
// returning an empty string when the line does not carry a parseable address.
func handshakeSourceIP(line string) string {
	i := strings.Index(line, handshakeLogMarker)
	if i < 0 {
		return ""
	}
	fields := strings.Fields(line[i+len(handshakeLogMarker):])
	if len(fields) == 0 {
		return ""
	}

	// net/http appends the error after "host:port:", so the address field
	// often ends in a colon as well.
	addr := strings.TrimRight(fields[0], ":")
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}
