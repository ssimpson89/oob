package bmc

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 websocket client: enough for an OpenBMC host console
// (bmcweb's /console0), which carries the serial line as binary messages both
// ways. No extensions, no compression; text and binary payloads are treated the
// same (raw bytes). The standard library has no client, and one endpoint does
// not justify a dependency.

// Opcodes (RFC 6455 section 5.2).
const (
	wsCont   = 0x0
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA
)

// wsMaxFrame bounds one incoming frame. A serial console sends small ones.
const wsMaxFrame = 1 << 20

// wsConn is an open websocket. Read returns message payloads as a byte stream;
// Write sends one binary message per call. Safe for one reader and many writers.
type wsConn struct {
	c  net.Conn
	br *bufio.Reader

	wmu    sync.Mutex
	closed bool

	cur io.Reader // remaining payload of the frame being read
}

// wsDial opens wss://addr/path over TLS with tc, sending hdr (for example an
// Authorization header) with the upgrade request.
func wsDial(ctx context.Context, addr, path string, tc *tls.Config, hdr http.Header) (*wsConn, error) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: redfishTimeout}, Config: tc}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	kb := make([]byte, 16)
	if _, err := rand.Read(kb); err != nil {
		raw.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(kb)
	host, _, _ := net.SplitHostPort(addr)
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n", path, host)
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	for k, vs := range hdr {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(raw, b.String()); err != nil {
		raw.Close()
		return nil, err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("websocket upgrade: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw.Close()
		return nil, &wsStatusError{code: resp.StatusCode, status: resp.Status}
	}
	// Sec-WebSocket-Accept is not checked against the RFC 6455 value: the NVIDIA
	// GB300 tray BMC (bmcweb, Boost.Beast/351) answers a valid 101 with a value
	// that does not match RFC 6455's (seen 2026-11-18), and browsers connect
	// anyway. The check guards against a non-websocket server answering; here
	// the TLS key is pinned and the 101 must still name websocket.
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		raw.Close()
		return nil, errors.New("websocket upgrade: the BMC's 101 answer is not a websocket upgrade")
	}
	_ = raw.SetDeadline(time.Time{})
	return &wsConn{c: raw, br: br}, nil
}

// wsStatusError is an upgrade the server refused.
type wsStatusError struct {
	code   int
	status string
}

func (e *wsStatusError) Error() string {
	if e.code == http.StatusUnauthorized || e.code == http.StatusForbidden {
		return "the BMC refused the console login (" + e.status + "): check the host's user and password"
	}
	return "the BMC refused the console websocket: " + e.status
}

// readHeader reads one frame header and returns its opcode, FIN bit and payload.
func (w *wsConn) readFrame() (op byte, fin bool, payload io.Reader, n int64, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n = int64(h[1] & 0x7f)
	switch n {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(w.br, e[:]); err != nil {
			return
		}
		n = int64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(w.br, e[:]); err != nil {
			return
		}
		n = int64(binary.BigEndian.Uint64(e[:]))
	}
	if n < 0 || n > wsMaxFrame {
		err = fmt.Errorf("websocket frame of %d bytes is too large", n)
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, mask[:]); err != nil {
			return
		}
	}
	lr := io.LimitReader(w.br, n)
	if masked {
		payload = &unmask{r: lr, key: mask}
	} else {
		payload = lr
	}
	return
}

type unmask struct {
	r   io.Reader
	key [4]byte
	i   int
}

func (u *unmask) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	for j := 0; j < n; j++ {
		p[j] ^= u.key[u.i%4]
		u.i++
	}
	return n, err
}

// Read returns the next bytes of the data messages. Control frames are handled
// here: ping is answered, close ends the stream with io.EOF.
func (w *wsConn) Read(p []byte) (int, error) {
	for {
		if w.cur != nil {
			n, err := w.cur.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil && err != io.EOF {
				return 0, err
			}
			w.cur = nil
		}
		op, _, payload, n, err := w.readFrame()
		if err != nil {
			return 0, err
		}
		switch op {
		case wsText, wsBinary, wsCont:
			w.cur = payload
		case wsPing:
			data, err := io.ReadAll(payload)
			if err != nil {
				return 0, err
			}
			_ = w.writeFrame(wsPong, data)
		case wsPong:
			_, _ = io.Copy(io.Discard, payload)
		case wsClose:
			_, _ = io.Copy(io.Discard, payload)
			_ = w.writeFrame(wsClose, nil)
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("websocket: unknown opcode %#x (%d bytes)", op, n)
		}
	}
}

// writeFrame sends one masked frame (clients must mask, RFC 6455 5.3).
func (w *wsConn) writeFrame(op byte, data []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed {
		return net.ErrClosed
	}
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	switch n := len(data); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	hdr = append(hdr, key[:]...)
	buf := make([]byte, len(hdr)+len(data))
	copy(buf, hdr)
	for i, c := range data {
		buf[len(hdr)+i] = c ^ key[i%4]
	}
	if op == wsClose {
		w.closed = true
	}
	_, err := w.c.Write(buf)
	return err
}

// Write sends p as one binary message.
func (w *wsConn) Write(p []byte) (int, error) {
	if err := w.writeFrame(wsBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseGracefully sends a close frame; Close drops the connection.
func (w *wsConn) CloseGracefully() error { return w.writeFrame(wsClose, []byte{0x03, 0xe8}) }

func (w *wsConn) Close() error { return w.c.Close() }
