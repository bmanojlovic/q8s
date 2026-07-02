package server

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
)

// WebSocket constants.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpBinary = byte(0x2)
	wsOpClose  = byte(0x8)
)

// Channel numbers defined by v4/v5.channel.k8s.io.
const (
	chanStdin  = byte(0)
	chanStdout = byte(1)
	chanStderr = byte(2)
	chanError  = byte(3)
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

func (s *Server) handlePodExec(w http.ResponseWriter, r *http.Request, ns, name string) {
	q := r.URL.Query()
	commands := q["command"]
	if len(commands) == 0 {
		s.respondStatus(w, http.StatusBadRequest, "BadRequest", "no command specified")
		return
	}
	wantStdin := q.Get("stdin") == "true"

	// Only GET + WebSocket upgrade is supported (kubectl ≥1.29 default).
	if r.Method != http.MethodGet {
		s.respondStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
			"exec requires a WebSocket upgrade (GET). Upgrade kubectl or pass --disable-http2")
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

	// Pick the best protocol the client offers.
	proto := ""
	for _, p := range strings.Split(r.Header.Get("Sec-Websocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if p == "v5.channel.k8s.io" || p == "v4.channel.k8s.io" {
			proto = p
			break
		}
	}

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

	// Build podman exec command.
	// Passing -t tells Podman to allocate a PTY inside the container so
	// interactive programs (bash, readline, vim) work correctly. Podman owns
	// the PTY master; we proxy bytes through pipes. Resize events (channel 4)
	// are accepted but silently ignored — the initial size is Podman's default.
	containerName := fmt.Sprintf("%s-%s", ns, name)
	wantTTY := q.Get("tty") == "true"
	podmanArgs := []string{"exec", "-i"}
	if wantTTY {
		podmanArgs = append(podmanArgs, "-t")
	}
	execArgs := append(append(podmanArgs, containerName), commands...)
	cmd := exec.Command("podman", execArgs...)

	var stdinW io.WriteCloser
	if wantStdin {
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
		ws.writeChan(chanError, execStatus(fmt.Errorf("exec failed: %w", err)))
		return
	}

	var wg sync.WaitGroup

	// stdout → channel 1
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(ws, chanStdout, stdoutR)
	}()

	// stderr → channel 2
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(ws, chanStderr, stderrR)
	}()

	// channel 0 → stdin (read WebSocket frames from client)
	if wantStdin {
		go func() {
			defer stdinW.Close()
			for {
				op, payload, err := readFrame(rw.Reader)
				if err != nil || op == wsOpClose {
					return
				}
				if op == wsOpBinary && len(payload) > 0 && payload[0] == chanStdin {
					stdinW.Write(payload[1:])
				}
				// channel 4 = resize — ignored (no PTY)
			}
		}()
	}

	wg.Wait()
	exitErr := cmd.Wait()

	// Send exit status on channel 3 then close the WebSocket.
	ws.writeChan(chanError, execStatus(exitErr))
	ws.writeFrame(wsOpClose, []byte{0x03, 0xE8}) // 1000 Normal Closure
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
