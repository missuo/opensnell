/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 *
 * Ported from open-snell.
 */

package http

import (
	"bufio"
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

type HTTPObfsServer struct {
	net.Conn
	buf           []byte
	bio           *bufio.Reader
	offset        int
	firstRequest  bool
	firstResponse bool
}

func (hos *HTTPObfsServer) Read(b []byte) (int, error) {
	if hos.buf != nil {
		n := copy(b, hos.buf[hos.offset:])
		hos.offset += n
		if hos.offset == len(hos.buf) {
			hos.offset = 0
			hos.buf = nil
		}
		return n, nil
	}

	if hos.firstRequest {
		bio := bufio.NewReader(hos.Conn)
		req, err := http.ReadRequest(bio)
		if err != nil {
			return 0, err
		}
		if req.Method != "GET" || req.Header.Get("Connection") != "Upgrade" {
			return 0, io.EOF
		}
		buf, err := io.ReadAll(req.Body)
		if err != nil {
			return 0, err
		}
		n := copy(b, buf)
		if n < len(buf) {
			hos.buf = buf
			hos.offset = n
		}
		_ = req.Body.Close()
		hos.bio = bio
		hos.firstRequest = false
		return n, nil
	}

	return hos.bio.Read(b)
}

const httpResponseTemplate = "HTTP/1.1 101 Switching Protocols\r\n" +
	"Server: nginx/1.%d.%d\r\n" +
	"Date: %s\r\n" +
	"Upgrade: websocket\r\n" +
	"Connection: Upgrade\r\n" +
	"Sec-WebSocket-Accept: %s\r\n" +
	"\r\n"

func (hos *HTTPObfsServer) Write(b []byte) (int, error) {
	if hos.firstResponse {
		randBytes := make([]byte, 16)
		_, _ = crand.Read(randBytes)
		date := time.Now().Format(time.RFC1123)
		vMajor := rand.IntN(11)
		vMinor := rand.IntN(12)
		resp := fmt.Sprintf(httpResponseTemplate, vMajor, vMinor, date,
			base64.URLEncoding.EncodeToString(randBytes))
		if _, err := hos.Conn.Write([]byte(resp)); err != nil {
			return 0, err
		}
		hos.firstResponse = false
		_, err := hos.Conn.Write(b)
		return len(b), err
	}
	return hos.Conn.Write(b)
}

func NewHTTPObfsServer(conn net.Conn) net.Conn {
	return &HTTPObfsServer{
		Conn:          conn,
		firstRequest:  true,
		firstResponse: true,
	}
}
