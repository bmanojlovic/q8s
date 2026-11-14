package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// WebSocket constants.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpBinary = byte(0x2)
	wsOpClose  = byte(0x8)
	wsOpPing   = byte(0x9)
	wsOpPong   = byte(0xA)
)

// wsKeepaliveInterval is how often the server sends a WebSocket ping on an
// exec/attach stream. Real k8s (kubelet) pings idle exec streams so an
// otherwise-quiet command doesn't trip a client's or an intermediary's
// (LB/proxy/NAT) idle-read timeout and tear the connection down. 20s is
// comfortably under common 30–60s idle windows. See tic-c767.
const wsKeepaliveInterval = 20 * time.Second

// wsReadTimeout is how long the server waits for any frame from an exec client
// before treating it as gone. Every frame resets the deadline; a live client
// answers the 20s server pings with pongs, so it never trips this.
const wsReadTimeout = 3 * wsKeepaliveInterval

// wsCloseWait is how long the server waits for the client to answer its
// WebSocket Close frame before it gives up and closes the connection. A var so
// tests can shorten it.
var wsCloseWait = 30 * time.Second

// wsCloseGrace is how long the frame reader gets to notice the read deadline
// that is pushed into the past when the close wait expires.
const wsCloseGrace = 250 * time.Millisecond

// Channel numbers defined by v4/v5.channel.k8s.io.
const (
	chanStdin  = byte(0)
	chanStdout = byte(1)
	chanStderr = byte(2)
	chanError  = byte(3)
	// chanClose is the v5 close signal: a binary frame [chanClose, stream]
	// tells the server that the client will send nothing more on that stream.
	chanClose = byte(255)
)

// execSessionEnv is set in the environment of every process of an exec
// session. Children inherit it, so a /proc scan can find all of them, which is
// what lets us stop the container processes when the client goes away.
const execSessionEnv = "Q8S_EXEC_SESSION"

const (
	// execKillGrace is how long a marked process gets to exit after SIGTERM
	// before it is SIGKILLed.
	execKillGrace = 5 * time.Second
	execKillPoll  = 100 * time.Millisecond
	// hostProcRoot is the procfs mount point; a variable so tests can point
	// the PID scan at a fake directory.
	hostProcRoot = "/proc"
)

// wsConn serialises writes to the hijacked WebSocket connection.
type wsConn struct {
	mu sync.Mutex
	w  io.Writer
}

func (c *wsConn) writeFrame(op byte, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hdr := []byte{0x80 | op, 0}
	n := len(payload)
	switch {
	case n <= 125:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(n))
		hdr = append(hdr, ext...)
	default:
		hdr[1] = 127
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append(hdr, ext...)
	}
	c.w.Write(hdr)
	c.w.Write(payload)
}

func (c *wsConn) writeChan(ch byte, data []byte) {
	msg := make([]byte, 1+len(data))
	msg[0] = ch
	copy(msg[1:], data)
	c.writeFrame(wsOpBinary, msg)
}

// ping sends a WebSocket ping control frame (no application payload). Goes
// through the same mutex as data frames, so it interleaves safely with
// stdout/stderr output.
func (c *wsConn) ping() { c.writeFrame(wsOpPing, nil) }

// pong replies to a client ping, echoing its payload as RFC 6455 requires.
func (c *wsConn) pong(payload []byte) { c.writeFrame(wsOpPong, payload) }

// wsCloseHandshake refreshes the client liveness deadline while the stream is
// open and stops doing so once the closing handshake timed out. The stop flag
// matters: without it a client that keeps sending pings would keep moving the
// deadline forward and the handler would never return.
type wsCloseHandshake struct {
	conn net.Conn

	mu      sync.Mutex
	stopped bool
}

// refreshReadDeadline extends the client liveness deadline. After
// expireReadDeadline it is a no-op. It holds the lock across the set so a
// refresh can never re-arm a deadline that was already pushed into the past.
func (h *wsCloseHandshake) refreshReadDeadline() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	_ = h.conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
}

// expireReadDeadline stops the deadline refreshes and pushes the read deadline
// into the past, so a reader blocked in readFrame returns at once.
func (h *wsCloseHandshake) expireReadDeadline() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	_ = h.conn.SetReadDeadline(time.Now())
}

// awaitClientClose finishes the RFC 6455 closing handshake after the server
// wrote its Close frame. TCP is ordered, so the client can only answer our
// Close once it has read everything sent before it, including the tail of a
// large stdout. Returning any earlier would let the kernel answer the client's
// still-arriving frames with a RST, which can destroy output the client has
// not read yet. readerDone reports that the frame reader already ended and its
// result was consumed by the caller, so there is nothing left to wait for.
func (h *wsCloseHandshake) awaitClientClose(framesDone <-chan bool, readerDone bool, timeout time.Duration) {
	if readerDone {
		return
	}
	select {
	case <-framesDone:
		return
	case <-time.After(timeout):
	}

	// The client never answered. Stop the reader itself: no more deadline
	// refreshes, and the deadline in the past unblocks a pending read.
	h.expireReadDeadline()
	select {
	case <-framesDone:
	case <-time.After(wsCloseGrace):
	}
}

// maxWSFrameBytes caps a single WebSocket frame. RFC 6455 lengths are 64-bit;
// allocating straight from the header would let an authenticated client ask
// for exabytes before sending a byte.
const maxWSFrameBytes = 1 << 20 // 1 MiB — stdin chunks from kubectl are far smaller

// readFrame reads one WebSocket frame from r. Returns opcode and unmasked payload.
func readFrame(r io.Reader) (op byte, payload []byte, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return
	}
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	plen := int64(hdr[1] & 0x7F)

	switch plen {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		plen = int64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		plen = int64(binary.BigEndian.Uint64(ext))
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(r, maskKey[:]); err != nil {
			return
		}
	}

	if plen > maxWSFrameBytes {
		return 0, nil, fmt.Errorf("websocket frame of %d bytes exceeds limit %d", plen, maxWSFrameBytes)
	}
	payload = make([]byte, plen)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return
}

// newExecSessionMarker returns a random 16 hex character session marker.
func newExecSessionMarker() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux; fall back to a unique-enough
		// value instead of failing the exec request.
		return fmt.Sprintf("%016x", uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}

// findMarkedPIDs returns the host PIDs whose /proc/<pid>/environ contains the
// exact NUL separated entry marker (for example "Q8S_EXEC_SESSION=abc"). Exact
// matching matters: "Q8S_EXEC_SESSION=abc" must not match the entry
// "Q8S_EXEC_SESSION=abcd". Unreadable and already-vanished pids are skipped.
func findMarkedPIDs(procRoot, marker string) []int {
	if marker == "" {
		return nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, entry := range bytes.Split(data, []byte{0}) {
			if string(entry) == marker {
				pids = append(pids, pid)
				break
			}
		}
	}
	sort.Ints(pids)
	return pids
}

// signalMarked sends sig to every process carrying the session marker.
func signalMarked(procRoot, marker string, sig syscall.Signal) {
	for _, pid := range findMarkedPIDs(procRoot, marker) {
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		_ = p.Signal(sig)
	}
}

// terminateExecSession stops every process of an exec session whose client has
// gone away. Killing the podman exec client does NOT stop the process inside
// the container (it is a child of a separate conmon), so the container side is
// signalled directly, using the session marker to find it. The podman exec
// client is killed last so the stdout/stderr pipes close and the handler
// returns.
func terminateExecSession(cmd *exec.Cmd, procRoot, marker string) {
	signalMarked(procRoot, marker, syscall.SIGTERM)

	deadline := time.Now().Add(execKillGrace)
	for time.Now().Before(deadline) {
		if len(findMarkedPIDs(procRoot, marker)) == 0 {
			break
		}
		time.Sleep(execKillPoll)
	}
	if len(findMarkedPIDs(procRoot, marker)) > 0 {
		signalMarked(procRoot, marker, syscall.SIGKILL)
	}

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// serveClientFrames reads client frames until the client goes away. It returns
// clientGone=true when the client closed the stream or the connection failed
// (including a read that hit the liveness deadline). The process stdin is
// closed on every return path, at most once. stdin may be nil when the exec
// request did not ask for stdin; channel 0 data is then dropped. setDeadline,
// when not nil, is called before every frame read to refresh the liveness
// deadline on the underlying connection.
func serveClientFrames(r io.Reader, setDeadline func(), stdin io.WriteCloser, ws *wsConn) (clientGone bool) {
	var closeOnce sync.Once
	closeStdin := func() {
		if stdin == nil {
			return
		}
		closeOnce.Do(func() { stdin.Close() })
	}
	defer closeStdin()

	for {
		if setDeadline != nil {
			setDeadline()
		}
		op, payload, err := readFrame(r)
		if err != nil {
			return true
		}
		switch op {
		case wsOpClose:
			return true
		case wsOpBinary:
			if len(payload) == 0 {
				continue
			}
			switch payload[0] {
			case chanStdin:
				if stdin != nil {
					if _, err := stdin.Write(payload[1:]); err != nil {
						return true
					}
				}
			case chanClose:
				// v5.channel.k8s.io: [255, stream] closes that stream. Only
				// stdin is meaningful here, and reading must continue because
				// pings and a later close frame still matter.
				if len(payload) >= 2 && payload[1] == chanStdin {
					closeStdin()
				}
			}
			// Channel 4 (resize) and unknown channels are ignored.
		case wsOpPing:
			// Answer a client keepalive so bidirectional idle pings don't
			// stall waiting for a pong.
			ws.pong(payload)
		case wsOpPong:
			// Response to our own ping; the deadline is already refreshed.
		}
	}
}

// podStreamTarget rejects a stream request for a pod that only exists as a
// CronJob run, whose container is already gone. A missing stored pod falls
// through and lets the podman command report the failure on the stream.
func (s *Server) podStreamTarget(w http.ResponseWriter, r *http.Request, ns, name, stream string) bool {
	if _, err := s.config.Store.GetPod(ns, name); err != nil {
		if _, ok := s.cronRunFindPod(r.Context(), ns, name); ok {
			s.respondStatus(w, http.StatusBadRequest, "BadRequest", "CronJob runs cannot be %s'd (the container is gone)", stream)
			return false
		}
	}
	return true
}

// execPodmanArgs builds the podman argv for an exec request. marker is the
// Q8S_EXEC_SESSION environment entry inherited by every process the exec
// starts, so a disconnected client can be cleaned up by scanning /proc. It is
// a pure function so the argv can be tested without podman.
func execPodmanArgs(containerName string, commands []string, wantTTY bool, marker string) []string {
	args := []string{"exec", "-i", "-e", marker}
	if wantTTY {
		args = append(args, "-t")
	}
	return append(append(args, containerName), commands...)
}

// attachPodmanArgs builds the podman argv for an attach request.
// --sig-proxy=false is deliberate: ending the kubectl session must detach,
// never signal the container. --no-stdin is added when the request did not
// ask for stdin, so attach does not wait on a stream it cannot receive.
func attachPodmanArgs(containerName string, wantStdin bool) []string {
	args := []string{"attach", "--sig-proxy=false"}
	if !wantStdin {
		args = append(args, "--no-stdin")
	}
	return append(args, containerName)
}

// negotiateWSProtocol picks the best channel protocol the client offers. The
// first supported entry wins, so a client that prefers v5 gets v5. An empty
// result means no supported protocol was offered and the handshake omits the
// Sec-WebSocket-Protocol response header.
func negotiateWSProtocol(header string) string {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if p == "v5.channel.k8s.io" || p == "v4.channel.k8s.io" {
			return p
		}
	}
	return ""
}

// podStream carries the process command and lifecycle differences between the
// exec and attach subresources into the shared WebSocket streaming code.
type podStream struct {
	// name is used in error messages: "exec" or "attach".
	name string
	// args is the complete podman argv.
	args []string
	// wantStdin opens the process stdin pipe and forwards channel 0 to it.
	wantStdin bool
	// marker is the exec session marker. It is empty for attach, where
	// terminating the podman attach process is enough to detach.
	marker string
}

// servePodStream upgrades the connection, proxies the v4/v5 channel protocol
// to a podman child process, and sends the exit status frame. It is shared by
// exec and attach; the command and cleanup semantics come from stream.
func (s *Server) servePodStream(w http.ResponseWriter, r *http.Request, stream podStream) {
	// Only GET + WebSocket upgrade is supported (kubectl 1.29 and newer
	// default to this transport).
	if r.Method != http.MethodGet {
		s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
			"%s requires a WebSocket upgrade (GET). Upgrade kubectl or pass --disable-http2", stream.name)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.respondStatus(w, http.StatusUpgradeRequired, "UpgradeRequired", "WebSocket upgrade required")
		return
	}

	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}

	proto := negotiateWSProtocol(r.Header.Get("Sec-Websocket-Protocol"))

	// Hijack the connection before writing the 101.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// WebSocket handshake.
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n"
	if proto != "" {
		resp += "Sec-WebSocket-Protocol: " + proto + "\r\n"
	}
	resp += "\r\n"
	rw.WriteString(resp)
	rw.Flush()

	ws := &wsConn{w: conn}
	cmd := exec.Command("podman", stream.args...)

	var stdinW io.WriteCloser
	if stream.wantStdin {
		stdinW, err = cmd.StdinPipe()
		if err != nil {
			ws.writeChan(chanError, execStatus(fmt.Errorf("stdin pipe: %w", err)))
			return
		}
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		ws.writeChan(chanError, execStatus(fmt.Errorf("stdout pipe: %w", err)))
		return
	}
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		ws.writeChan(chanError, execStatus(fmt.Errorf("stderr pipe: %w", err)))
		return
	}

	if err := cmd.Start(); err != nil {
		ws.writeChan(chanError, execStatus(fmt.Errorf("%s failed: %w", stream.name, err)))
		return
	}

	var wg sync.WaitGroup

	// Keepalive: ping the client on an interval so an idle command (no
	// stdout/stderr for a while) does not let a client or intermediary idle-
	// read timeout tear the stream down. Stopped when the command finishes.
	// writeFrame is mutex-guarded, so pings interleave safely with output.
	keepaliveDone := make(chan struct{})
	go func() {
		t := time.NewTicker(wsKeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-keepaliveDone:
				return
			case <-t.C:
				ws.ping()
			}
		}
	}()

	// stdout -> channel 1
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(ws, chanStdout, stdoutR)
	}()

	// stderr -> channel 2
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(ws, chanStderr, stderrR)
	}()

	// Client frames are always read, even without stdin: a v5 close frame, a
	// close frame or a dead connection must be noticed in every mode. Every
	// frame also refreshes the read deadline, which is the client liveness
	// check. stdinW is nil when the request did not ask for stdin.
	closeHandshake := &wsCloseHandshake{conn: conn}
	framesDone := make(chan bool, 1)
	go func() {
		framesDone <- serveClientFrames(rw.Reader, closeHandshake.refreshReadDeadline, stdinW, ws)
	}()

	// Wait for the stdout/stderr pipes to close (which happens when the
	// command exits) or for the client to go away. A client that disappears
	// while the command still runs is a genuine disconnect: stop the session.
	// Once the pipes are done the command has exited on its own, so a reader
	// that ends later (because the handler closes the connection) must not
	// terminate anything.
	cmdDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(cmdDone)
	}()

	readerDone := false
	select {
	case <-cmdDone:
	case gone := <-framesDone:
		readerDone = true
		if gone {
			select {
			case <-cmdDone:
				// The command exited at the same time; nothing to clean up.
			default:
				// A dead peer can leave an output goroutine blocked in
				// conn.Write (holding the frame mutex) once the TCP send
				// buffer fills; make further writes fail at once so the
				// pipes drain and the handler can return.
				_ = conn.SetWriteDeadline(time.Now())
				if stream.marker != "" {
					terminateExecSession(cmd, hostProcRoot, stream.marker)
				} else if cmd.Process != nil {
					// Attach needs no session marker: killing the podman
					// attach client only detaches from the container.
					_ = cmd.Process.Kill()
				}
			}
		}
		<-cmdDone
	}
	close(keepaliveDone)
	exitErr := cmd.Wait()

	// Send exit status on channel 3 then close the WebSocket.
	ws.writeChan(chanError, execStatus(exitErr))
	ws.writeFrame(wsOpClose, []byte{0x03, 0xE8}) // 1000 Normal Closure

	// Wait for the client's side of the close handshake before returning: the
	// deferred conn.Close would otherwise reset a connection the client is
	// still using to finish reading stdout.
	closeHandshake.awaitClientClose(framesDone, readerDone, wsCloseWait)
}

func (s *Server) handlePodExec(w http.ResponseWriter, r *http.Request, ns, name string) {
	if !s.podStreamTarget(w, r, ns, name, "exec") {
		return
	}
	q := r.URL.Query()
	commands := q["command"]
	if len(commands) == 0 {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "no command specified")
		return
	}
	wantStdin := q.Get("stdin") == "true"

	// Passing -t tells Podman to allocate a PTY inside the container so
	// interactive programs (bash, readline, vim) work correctly. Podman owns
	// the PTY master; we proxy bytes through pipes. Resize events (channel 4)
	// are accepted but silently ignored: the initial size is Podman's default.
	containerName := s.podContainerName(ns, name, q.Get("container"))
	wantTTY := q.Get("tty") == "true"
	// The session marker is inherited by every process podman starts for this
	// exec, so we can find and stop them all if the client goes away.
	marker := execSessionEnv + "=" + newExecSessionMarker()

	s.servePodStream(w, r, podStream{
		name:      "exec",
		args:      execPodmanArgs(containerName, commands, wantTTY, marker),
		wantStdin: wantStdin,
		marker:    marker,
	})
}

// handlePodAttach streams an existing container to pods/POD/attach. It uses
// the same protocol and connection handling as exec, but podman attach only
// mirrors the container's stdio and never signals it on exit.
func (s *Server) handlePodAttach(w http.ResponseWriter, r *http.Request, ns, name string) {
	if !s.podStreamTarget(w, r, ns, name, "attach") {
		return
	}
	q := r.URL.Query()
	containerName := s.podContainerName(ns, name, q.Get("container"))
	wantStdin := q.Get("stdin") == "true"

	s.servePodStream(w, r, podStream{
		name:      "attach",
		args:      attachPodmanArgs(containerName, wantStdin),
		wantStdin: wantStdin,
	})
}

func pipe(ws *wsConn, ch byte, r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			ws.writeChan(ch, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// execStatusBody is the JSON payload kubectl parses on channel 3.
type execStatusBody struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Status     string             `json:"status"`
	Message    string             `json:"message,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Details    *execStatusDetails `json:"details,omitempty"`
	Code       int                `json:"code"`
}

type execStatusDetails struct {
	Causes []execStatusCause `json:"causes"`
}

type execStatusCause struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func execStatus(exitErr error) []byte {
	var body execStatusBody
	if exitErr == nil {
		body = execStatusBody{APIVersion: "v1", Kind: "Status", Status: "Success", Code: 200}
	} else {
		code := 1
		if ee, ok := exitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		body = execStatusBody{
			APIVersion: "v1",
			Kind:       "Status",
			Status:     "Failure",
			Message:    fmt.Sprintf("command terminated with exit code %d", code),
			Reason:     "NonZeroExitCode",
			Details: &execStatusDetails{
				Causes: []execStatusCause{{Reason: "ExitCode", Message: fmt.Sprintf("%d", code)}},
			},
			Code: 500,
		}
	}
	b, _ := json.Marshal(body)
	return b
}
