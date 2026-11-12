package server

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for the handshake filter tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestHandshakeFilterDropsHandshakesAndSummarizes(t *testing.T) {
	clock := newFakeClock()
	var buf bytes.Buffer
	f := newHandshakeFilter(&buf, clock.now)

	handshakes := []string{
		"http: TLS handshake error from 192.0.2.10:51234: EOF\n",
		"http: TLS handshake error from 198.51.100.7:51240: tls: client offered only unsupported versions: []\n",
		"http: TLS handshake error from 203.0.113.9:51244: tls: client didn't provide a certificate\n",
	}
	for _, line := range handshakes {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatalf("Write(%q): %v", line, err)
		}
	}

	if buf.Len() != 0 {
		t.Fatalf("handshake lines reached the destination: %q", buf.String())
	}
	if f.suppressed != len(handshakes) {
		t.Fatalf("suppressed = %d, want %d", f.suppressed, len(handshakes))
	}
	if f.lastIP != "203.0.113.9" {
		t.Fatalf("lastIP = %q, want %q", f.lastIP, "203.0.113.9")
	}

	// Just before the interval: no summary yet.
	clock.advance(handshakeSummaryInterval - time.Nanosecond)
	real := "http: proxy error: dial tcp 192.0.2.20:8080: connect: connection refused\n"
	if _, err := f.Write([]byte(real)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), real; got != want {
		t.Fatalf("output before interval = %q, want %q", got, want)
	}

	// One more nanosecond: the next line triggers exactly one summary, placed
	// before the line itself, then the count resets.
	clock.advance(time.Nanosecond)
	if _, err := f.Write([]byte(real)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	summary := "tls: 3 failed handshakes suppressed in the last 10m (last from 203.0.113.9)\n"
	if got, want := buf.String(), real+summary+real; got != want {
		t.Fatalf("output after interval = %q, want %q", got, want)
	}
	if f.suppressed != 0 {
		t.Fatalf("suppressed after summary = %d, want 0", f.suppressed)
	}
	if f.lastIP != "" {
		t.Fatalf("lastIP after summary = %q, want empty", f.lastIP)
	}

	// Nothing was suppressed since the summary, so no second one appears even
	// after another full interval.
	clock.advance(2 * handshakeSummaryInterval)
	if _, err := f.Write([]byte(real)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), real+summary+real+real; got != want {
		t.Fatalf("output after empty interval = %q, want %q", got, want)
	}
}

func TestHandshakeFilterSummarizesAtMostOncePerInterval(t *testing.T) {
	clock := newFakeClock()
	var buf bytes.Buffer
	f := newHandshakeFilter(&buf, clock.now)

	handshake := "http: TLS handshake error from 192.0.2.30:44444: EOF\n"
	for i := 0; i < 5; i++ {
		if _, err := f.Write([]byte(handshake)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	clock.advance(handshakeSummaryInterval)
	// Several lines arrive in the same instant: only the first one may carry
	// the summary.
	for i := 0; i < 3; i++ {
		if _, err := f.Write([]byte("ok\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	want := "tls: 5 failed handshakes suppressed in the last 10m (last from 192.0.2.30)\n" + "ok\nok\nok\n"
	if got := buf.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestHandshakeFilterPassesOtherLinesVerbatim(t *testing.T) {
	clock := newFakeClock()
	var buf bytes.Buffer
	f := newHandshakeFilter(&buf, clock.now)

	in := "http: superfluous response.WriteHeader call from q8s\n" +
		"\n" +
		"unterminated line without newline"
	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); got != in {
		t.Fatalf("output = %q, want %q", got, in)
	}

	// The marker is matched anywhere in the line, not only at its start.
	if _, err := f.Write([]byte("prefix http: TLS handshake error from 192.0.2.40:1234: EOF\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); got != in {
		t.Fatalf("output after marked line = %q, want %q", got, in)
	}
	if f.suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", f.suppressed)
	}
}

func TestHandshakeFilterConcurrentWrites(t *testing.T) {
	clock := newFakeClock()
	var buf bytes.Buffer
	f := newHandshakeFilter(&buf, clock.now)

	const (
		writers   = 16
		perWriter = 100
	)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if j%2 == 0 {
					line := fmt.Sprintf("http: TLS handshake error from 192.0.2.%d:%d: EOF\n", i+1, 40000+j)
					if _, err := f.Write([]byte(line)); err != nil {
						t.Errorf("Write handshake: %v", err)
						return
					}
					continue
				}
				line := fmt.Sprintf("plain line %d %d\n", i, j)
				if _, err := f.Write([]byte(line)); err != nil {
					t.Errorf("Write plain: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	const handshakeCount = writers * perWriter / 2
	if f.suppressed != handshakeCount {
		t.Fatalf("suppressed = %d, want %d", f.suppressed, handshakeCount)
	}
	if strings.Contains(buf.String(), handshakeLogMarker) {
		t.Fatalf("handshake line leaked into output: %q", buf.String())
	}
	if got := strings.Count(buf.String(), "plain line "); got != handshakeCount {
		t.Fatalf("plain line count = %d, want %d", got, handshakeCount)
	}

	clock.advance(handshakeSummaryInterval)
	if _, err := f.Write([]byte("after\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	summary := fmt.Sprintf("tls: %d failed handshakes suppressed in the last 10m (last from 192.0.2.", handshakeCount)
	if !strings.Contains(buf.String(), summary) {
		t.Fatalf("summary %q missing from output (len %d)", summary, buf.Len())
	}
}
