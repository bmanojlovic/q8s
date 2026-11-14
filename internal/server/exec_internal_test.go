package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReadFrameRejectsOversizedLength pins the frame cap: a client
// advertising a huge 64-bit length must get an error, not a huge allocation.
func TestReadFrameRejectsOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x82) // FIN + binary
	buf.WriteByte(0xFF) // masked + 7-bit length code 127 (64-bit length)
	var ext [8]byte
	binary.BigEndian.PutUint64(ext[:], 1<<40) // 1 TiB
	buf.Write(ext[:])
	buf.Write([]byte{1, 2, 3, 4}) // mask key

	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("expected error for oversized frame length")
	} else if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReadFrameAcceptsSmallFrame: the cap must not break normal frames.
func TestReadFrameAcceptsSmallFrame(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{chanStdin, 'h', 'i'}
	buf.WriteByte(0x82)                      // FIN + binary
	buf.WriteByte(byte(0x80 | len(payload))) // masked
	buf.Write([]byte{0, 0, 0, 0})            // zero mask = identity
	buf.Write(payload)

	op, got, err := readFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if op != wsOpBinary || !bytes.Equal(got, payload) {
		t.Errorf("got op=%d payload=%q", op, got)
	}
}

// clientFrame builds a masked client frame, the way kubectl sends them.
func clientFrame(op byte, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x80 | op) // FIN + opcode
	n := len(payload)
	if n <= 125 {
		buf.WriteByte(0x80 | byte(n)) // masked
	} else {
		buf.WriteByte(0x80 | 126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		buf.Write(ext[:])
	}
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	buf.Write(mask[:])
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	buf.Write(masked)
	return buf.Bytes()
}

// recordingWriteCloser stands in for the process stdin.
type recordingWriteCloser struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closes int
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closes++
	return nil
}

func (w *recordingWriteCloser) written() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *recordingWriteCloser) closeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closes
}

// lockedBuffer is a bytes.Buffer safe to read while the frame loop writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// eventually polls cond until it is true or the test times out. The frame loop
// runs concurrently with the test, so states are awaited rather than slept on.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestServeClientFramesCloseSignalClosesStdinAndKeepsReading covers the v5
// stdin EOF signal: [255, 0] closes the process stdin, and the loop must keep
// reading afterwards (here: a ping still gets a pong).
func TestServeClientFramesCloseSignalClosesStdinAndKeepsReading(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	var out lockedBuffer
	ws := &wsConn{w: &out}
	stdin := &recordingWriteCloser{}
	done := make(chan bool, 1)
	go func() { done <- serveClientFrames(pr, nil, stdin, ws) }()

	if _, err := pw.Write(clientFrame(wsOpBinary, []byte{chanStdin, 'h', 'i'})); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(clientFrame(wsOpBinary, []byte{chanClose, chanStdin})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "stdin to be closed by [255, 0]", func() bool {
		return stdin.closeCount() == 1
	})

	// The loop must still be reading: a ping after the close gets a pong.
	pingPayload := []byte("ka")
	if _, err := pw.Write(clientFrame(wsOpPing, pingPayload)); err != nil {
		t.Fatal(err)
	}
	wantPong := append([]byte{0x80 | wsOpPong, byte(len(pingPayload))}, pingPayload...)
	eventually(t, "pong for a ping sent after the stdin close", func() bool {
		return bytes.Contains(out.bytes(), wantPong)
	})

	if got := stdin.written(); got != "hi" {
		t.Errorf("stdin received %q, want %q", got, "hi")
	}
	if n := stdin.closeCount(); n != 1 {
		t.Errorf("stdin closed %d times, want 1", n)
	}

	// Ending the client stream reports the client as gone and closes stdin
	// once more at most.
	pw.Close()
	select {
	case gone := <-done:
		if !gone {
			t.Error("serveClientFrames returned clientGone=false after EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveClientFrames did not return after the client stream ended")
	}
	if n := stdin.closeCount(); n != 1 {
		t.Errorf("stdin closed %d times, want 1", n)
	}
}

// TestServeClientFramesIgnoresOtherCloseStreams: [255, 2] closes stderr on the
// client side and must not close the process stdin.
func TestServeClientFramesIgnoresOtherCloseStreams(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	stdin := &recordingWriteCloser{}
	done := make(chan bool, 1)
	go func() { done <- serveClientFrames(pr, nil, stdin, &wsConn{w: io.Discard}) }()

	if _, err := pw.Write(clientFrame(wsOpBinary, []byte{chanClose, chanStderr})); err != nil {
		t.Fatal(err)
	}
	// Frames are handled in order, so once this data has arrived the close
	// frame for stderr has already been processed.
	if _, err := pw.Write(clientFrame(wsOpBinary, []byte{chanStdin, 'x'})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "stdin data to arrive", func() bool { return stdin.written() == "x" })

	if n := stdin.closeCount(); n != 0 {
		t.Errorf("stdin closed %d times after [255, 2], want 0", n)
	}

	pw.Close()
	select {
	case gone := <-done:
		if !gone {
			t.Error("serveClientFrames returned clientGone=false after EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveClientFrames did not return after the client stream ended")
	}
	// The reader ending for any reason closes stdin exactly once.
	if n := stdin.closeCount(); n != 1 {
		t.Errorf("stdin closed %d times, want 1", n)
	}
}

// TestServeClientFramesCloseFrameEndsLoop: a WebSocket close frame means the
// client is gone and the stdin pipe must be closed.
func TestServeClientFramesCloseFrameEndsLoop(t *testing.T) {
	stdin := &recordingWriteCloser{}
	done := make(chan bool, 1)
	go func() {
		done <- serveClientFrames(bytes.NewReader(clientFrame(wsOpClose, []byte{0x03, 0xE8})), nil, stdin, &wsConn{w: io.Discard})
	}()

	select {
	case gone := <-done:
		if !gone {
			t.Error("serveClientFrames returned clientGone=false for a close frame")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveClientFrames did not return for a close frame")
	}
	if n := stdin.closeCount(); n != 1 {
		t.Errorf("stdin closed %d times, want 1", n)
	}
}

// TestServeClientFramesEOFIsClientGone: the reader ending without a close frame
// (connection reset, read deadline, ...) is also "client gone".
func TestServeClientFramesEOFIsClientGone(t *testing.T) {
	stdin := &recordingWriteCloser{}
	done := make(chan bool, 1)
	go func() {
		done <- serveClientFrames(bytes.NewReader(nil), func() {}, stdin, &wsConn{w: io.Discard})
	}()

	select {
	case gone := <-done:
		if !gone {
			t.Error("serveClientFrames returned clientGone=false after EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveClientFrames did not return after EOF")
	}
	if n := stdin.closeCount(); n != 1 {
		t.Errorf("stdin closed %d times, want 1", n)
	}
}

// TestServeClientFramesWithoutStdin: when the exec request did not ask for
// stdin, channel 0 data is dropped without a panic.
func TestServeClientFramesWithoutStdin(t *testing.T) {
	frames := append(clientFrame(wsOpBinary, []byte{chanStdin, 'h', 'i'}),
		clientFrame(wsOpBinary, []byte{chanClose, chanStdin})...)
	frames = append(frames, clientFrame(wsOpClose, []byte{0x03, 0xE8})...)

	done := make(chan bool, 1)
	go func() {
		done <- serveClientFrames(bytes.NewReader(frames), nil, nil, &wsConn{w: io.Discard})
	}()

	select {
	case gone := <-done:
		if !gone {
			t.Error("serveClientFrames returned clientGone=false after a close frame")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveClientFrames did not return")
	}
}

// TestServeClientFramesRefreshesDeadline: the read deadline hook runs before
// every frame read, so a pong to our own ping extends the client liveness
// window.
func TestServeClientFramesRefreshesDeadline(t *testing.T) {
	frames := append(clientFrame(wsOpPong, nil), clientFrame(wsOpPong, nil)...)

	var mu sync.Mutex
	calls := 0
	done := make(chan bool, 1)
	go func() {
		done <- serveClientFrames(bytes.NewReader(frames), func() {
			mu.Lock()
			calls++
			mu.Unlock()
		}, nil, &wsConn{w: io.Discard})
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	// Two frames plus the read that hits EOF.
	if calls != 3 {
		t.Errorf("setDeadline called %d times, want 3", calls)
	}
}

// TestWsCloseHandshakeWaitsForClientClose: after writing its Close frame the
// server must keep the stream open until the client answers with its own Close
// frame, so the client can still read the tail of a large stdout.
func TestWsCloseHandshakeWaitsForClientClose(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	oldWait := wsCloseWait
	wsCloseWait = 5 * time.Second
	t.Cleanup(func() { wsCloseWait = oldWait })

	hs := &wsCloseHandshake{conn: server}
	ws := &wsConn{w: server}
	framesDone := make(chan bool, 1)
	go func() {
		framesDone <- serveClientFrames(server, hs.refreshReadDeadline, nil, ws)
	}()

	// The client consumes every frame the server sends and reports the Close
	// frame it is expected to answer.
	sawClose := make(chan struct{})
	go func() {
		for {
			op, _, err := readFrame(client)
			if err != nil {
				return
			}
			if op == wsOpClose {
				close(sawClose)
				return
			}
		}
	}()

	// What servePodStream writes at the end of the stream. It shares the
	// wsConn with the frame reader, as the real handler does, so the pong
	// replies of the reader and these frames cannot interleave.
	ws.writeChan(chanError, execStatus(nil))
	ws.writeFrame(wsOpClose, []byte{0x03, 0xE8})
	<-sawClose

	waited := make(chan struct{})
	go func() {
		hs.awaitClientClose(framesDone, false, wsCloseWait)
		close(waited)
	}()

	// The client has not answered yet, so the handshake must still be running
	// (and the handler must not have closed the connection).
	select {
	case <-waited:
		t.Fatal("close handshake returned before the client sent its Close frame")
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := client.Write(clientFrame(wsOpClose, []byte{0x03, 0xE8})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("close handshake did not return after the client sent its Close frame")
	}
}

// TestWsCloseHandshakeCutsOffUnansweringClient: a client that never answers
// the server's Close frame is cut off after wsCloseWait even while it keeps
// sending pings, which must not refresh the read deadline any more.
func TestWsCloseHandshakeCutsOffUnansweringClient(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	oldWait := wsCloseWait
	wsCloseWait = 200 * time.Millisecond
	t.Cleanup(func() { wsCloseWait = oldWait })

	hs := &wsCloseHandshake{conn: server}
	ws := &wsConn{w: server}
	framesDone := make(chan bool, 1)
	readerDone := make(chan struct{})
	go func() {
		framesDone <- serveClientFrames(server, hs.refreshReadDeadline, nil, ws)
		close(readerDone)
	}()

	// The client ignores the server's Close frame and keeps sending pings.
	// Reading and writing are separate goroutines because a net.Pipe write
	// blocks until it is read, so one goroutine doing both would deadlock
	// against the pongs the server writes. The ping payload is not empty
	// because net.Pipe blocks on a zero-length write.
	clientDone := make(chan struct{})
	go func() {
		for {
			if _, _, err := readFrame(client); err != nil {
				break
			}
		}
		close(clientDone)
	}()
	go func() {
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for range t.C {
			if _, err := client.Write(clientFrame(wsOpPing, []byte("ka"))); err != nil {
				return
			}
		}
	}()

	ws.writeChan(chanError, execStatus(nil))
	ws.writeFrame(wsOpClose, []byte{0x03, 0xE8})

	start := time.Now()
	hs.awaitClientClose(framesDone, false, wsCloseWait)
	elapsed := time.Since(start)

	if elapsed < wsCloseWait {
		t.Errorf("close handshake returned after %v, before the %v wait", elapsed, wsCloseWait)
	}
	if elapsed > 2*time.Second {
		t.Errorf("close handshake took %v, want about %v", elapsed, wsCloseWait)
	}

	// The read deadline push must end the frame reader despite the pings still
	// arriving; otherwise the pings kept extending the liveness deadline.
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("frame reader kept running after the close wait expired")
	}

	_ = client.Close()
	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("client loop did not stop")
	}
}

// TestWsCloseHandshakeStopsRefreshingDeadline: once the close wait expired, a
// deadline refresh must not move the read deadline forward again, otherwise a
// client that keeps sending pings would keep the frame reader alive forever.
func TestWsCloseHandshakeStopsRefreshingDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	hs := &wsCloseHandshake{conn: server}
	hs.expireReadDeadline()
	hs.refreshReadDeadline()

	// The deadline is still in the past, so a read must fail at once instead
	// of blocking until wsReadTimeout.
	done := make(chan error, 1)
	go func() {
		_, _, err := readFrame(server)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read after an expired close deadline did not fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refreshReadDeadline moved the deadline forward after it expired")
	}
}

// TestFindMarkedPIDsMatchesExactEnvironEntry covers the /proc scan used to
// terminate the container processes of a disconnected exec session.
func TestFindMarkedPIDsMatchesExactEnvironEntry(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"1/environ": "A=1\x00Q8S_EXEC_SESSION=abc\x00",
		"2/environ": "Q8S_EXEC_SESSION=abcd",
		"3/environ": "PATH=/usr/bin\x00HOME=/tmp",
		"self":      "not a pid dir",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A pid directory without an environ file (vanished process) is skipped.
	if err := os.MkdirAll(filepath.Join(root, "4"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := findMarkedPIDs(root, "Q8S_EXEC_SESSION=abc")
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("findMarkedPIDs = %v, want [1]", got)
	}

	if got := findMarkedPIDs(root, "Q8S_EXEC_SESSION=abcd"); len(got) != 1 || got[0] != 2 {
		t.Errorf("findMarkedPIDs for abcd = %v, want [2]", got)
	}
	if got := findMarkedPIDs(root, ""); len(got) != 0 {
		t.Errorf("findMarkedPIDs with empty marker = %v, want none", got)
	}
	if got := findMarkedPIDs(filepath.Join(root, "missing"), "Q8S_EXEC_SESSION=abc"); len(got) != 0 {
		t.Errorf("findMarkedPIDs with a missing proc root = %v, want none", got)
	}
}

// TestNewExecSessionMarker pins the marker format: 16 lowercase hex chars.
func TestNewExecSessionMarker(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		m := newExecSessionMarker()
		if len(m) != 16 {
			t.Fatalf("marker %q has length %d, want 16", m, len(m))
		}
		for _, c := range m {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("marker %q is not lowercase hex", m)
			}
		}
		if seen[m] {
			t.Fatalf("marker %q repeated", m)
		}
		seen[m] = true
	}
}
