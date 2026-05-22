/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tls

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"time"
)

type TLSObfsServer struct {
	net.Conn
	remain            int
	firstRequest      bool
	sessionTicketDone bool
	firstResponse     bool
}

func (tos *TLSObfsServer) read(b []byte, skipSize int) (int, error) {
	r, n, err := readBlock(tos.Conn, b, skipSize)
	tos.remain = r
	return n, err
}

func (tos *TLSObfsServer) skipOtherExts() error {
	buf := make([]byte, 256)
	if _, err := tos.read(buf, 7); err != nil {
		return err
	}
	_, err := io.ReadFull(tos.Conn, buf[:4*16+2])
	return err
}

func (tos *TLSObfsServer) Read(b []byte) (int, error) {
	if tos.remain > 0 {
		length := tos.remain
		if length > len(b) {
			length = len(b)
		}
		n, err := io.ReadFull(tos.Conn, b[:length])
		tos.remain -= n
		return n, err
	}

	if tos.firstRequest {
		tos.firstRequest = false
		return tos.read(b, 9*16-4)
	}

	if !tos.sessionTicketDone {
		tos.sessionTicketDone = true
		if err := tos.skipOtherExts(); err != nil {
			return 0, err
		}
	}
	return tos.read(b, 3)
}

func (tos *TLSObfsServer) Write(b []byte) (int, error) {
	for i := 0; i < len(b); i += chunkSize {
		end := i + chunkSize
		if end > len(b) {
			end = len(b)
		}
		if n, err := tos.write(b[i:end]); err != nil {
			return n, err
		}
	}
	return len(b), nil
}

func (tos *TLSObfsServer) write(b []byte) (int, error) {
	if tos.firstResponse {
		serverHello := makeServerHello(b)
		_, err := tos.Conn.Write(serverHello)
		tos.firstResponse = false
		return len(b), err
	}
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	buf.Write([]byte{0x17, 0x03, 0x03})
	_ = binary.Write(buf, binary.BigEndian, uint16(len(b)))
	if _, err := tos.Conn.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return tos.Conn.Write(b)
}

func NewTLSObfsServer(conn net.Conn) net.Conn {
	return &TLSObfsServer{
		Conn:          conn,
		firstRequest:  true,
		firstResponse: true,
	}
}

func makeServerHello(data []byte) []byte {
	randBytes := make([]byte, 28)
	sessionID := make([]byte, 32)
	_, _ = rand.Read(randBytes)
	_, _ = rand.Read(sessionID)

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	buf.WriteByte(0x16)
	_ = binary.Write(buf, binary.BigEndian, uint16(0x0301))
	_ = binary.Write(buf, binary.BigEndian, uint16(91))
	buf.Write([]byte{2, 0, 0, 87, 0x03, 0x03})
	_ = binary.Write(buf, binary.BigEndian, uint32(time.Now().Unix()))
	buf.Write(randBytes)
	buf.WriteByte(32)
	buf.Write(sessionID)

	buf.Write([]byte{0xcc, 0xa8})
	buf.WriteByte(0)
	buf.Write([]byte{0x00, 0x00})
	buf.Write([]byte{0xff, 0x01, 0x00, 0x01, 0x00})
	buf.Write([]byte{0x00, 0x17, 0x00, 0x00})
	buf.Write([]byte{0x00, 0x0b, 0x00, 0x02, 0x01, 0x00})
	buf.Write([]byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01})
	buf.Write([]byte{0x16, 0x03, 0x03})
	_ = binary.Write(buf, binary.BigEndian, uint16(len(data)))
	buf.Write(data)

	return buf.Bytes()
}
