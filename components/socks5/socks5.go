/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 *
 * Address handling and handshake logic ported from
 * github.com/icpz/open-snell and the Dreamacro/clash project.
 */

package socks5

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
)

type Error byte

func (err Error) Error() string {
	return "SOCKS error: " + strconv.Itoa(int(err))
}

type Command = uint8

const (
	CmdConnect      Command = 1
	CmdBind         Command = 2
	CmdUDPAssociate Command = 3
)

const (
	AtypIPv4       = 1
	AtypDomainName = 3
	AtypIPv6       = 4
)

const MaxAddrLen = 1 + 1 + 255 + 2

type Addr []byte

func (a Addr) String() string {
	var host, port string
	switch a[0] {
	case AtypDomainName:
		hostLen := uint16(a[1])
		host = string(a[2 : 2+hostLen])
		port = strconv.Itoa((int(a[2+hostLen]) << 8) | int(a[2+hostLen+1]))
	case AtypIPv4:
		host = net.IP(a[1 : 1+net.IPv4len]).String()
		port = strconv.Itoa((int(a[1+net.IPv4len]) << 8) | int(a[1+net.IPv4len+1]))
	case AtypIPv6:
		host = net.IP(a[1 : 1+net.IPv6len]).String()
		port = strconv.Itoa((int(a[1+net.IPv6len]) << 8) | int(a[1+net.IPv6len+1]))
	}
	return net.JoinHostPort(host, port)
}

// HostPort returns the host and port components separately. Useful when
// translating to the snell wire format which carries them as two fields.
func (a Addr) HostPort() (host string, port uint16) {
	switch a[0] {
	case AtypDomainName:
		hostLen := uint16(a[1])
		host = string(a[2 : 2+hostLen])
		port = binary.BigEndian.Uint16(a[2+hostLen : 2+hostLen+2])
	case AtypIPv4:
		host = net.IP(a[1 : 1+net.IPv4len]).String()
		port = binary.BigEndian.Uint16(a[1+net.IPv4len:])
	case AtypIPv6:
		host = net.IP(a[1 : 1+net.IPv6len]).String()
		port = binary.BigEndian.Uint16(a[1+net.IPv6len:])
	}
	return
}

func (a Addr) UDPAddr() *net.UDPAddr {
	if len(a) == 0 {
		return nil
	}
	switch a[0] {
	case AtypIPv4:
		return &net.UDPAddr{
			IP:   net.IP(a[1 : 1+net.IPv4len]),
			Port: int(binary.BigEndian.Uint16(a[1+net.IPv4len:])),
		}
	case AtypIPv6:
		return &net.UDPAddr{
			IP:   net.IP(a[1 : 1+net.IPv6len]),
			Port: int(binary.BigEndian.Uint16(a[1+net.IPv6len:])),
		}
	}
	return nil
}

const (
	ErrGeneralFailure       = Error(1)
	ErrConnectionNotAllowed = Error(2)
	ErrNetworkUnreachable   = Error(3)
	ErrHostUnreachable      = Error(4)
	ErrConnectionRefused    = Error(5)
	ErrTTLExpired           = Error(6)
	ErrCommandNotSupported  = Error(7)
	ErrAddressNotSupported  = Error(8)
)

// ServerHandshake completes the SOCKS5 negotiation on rw and returns the
// requested target address and command. Authentication is not supported.
func ServerHandshake(rw net.Conn) (addr Addr, command Command, err error) {
	buf := make([]byte, MaxAddrLen)

	// VER, NMETHODS, METHODS
	if _, err = io.ReadFull(rw, buf[:2]); err != nil {
		return
	}
	nmethods := buf[1]
	if _, err = io.ReadFull(rw, buf[:nmethods]); err != nil {
		return
	}
	if _, err = rw.Write([]byte{5, 0}); err != nil {
		return
	}

	// VER CMD RSV ATYP DST.ADDR DST.PORT
	if _, err = io.ReadFull(rw, buf[:3]); err != nil {
		return
	}
	command = buf[1]
	addr, err = ReadAddr(rw, buf)
	if err != nil {
		return
	}

	switch command {
	case CmdConnect, CmdUDPAssociate:
		localAddr := ParseAddr(rw.LocalAddr().String())
		if localAddr == nil {
			err = ErrAddressNotSupported
			return
		}
		_, err = rw.Write(bytes.Join([][]byte{{5, 0, 0}, localAddr}, nil))
	default:
		err = ErrCommandNotSupported
	}
	return
}

func ReadAddr(r io.Reader, b []byte) (Addr, error) {
	if len(b) < MaxAddrLen {
		return nil, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return nil, err
	}

	switch b[0] {
	case AtypDomainName:
		if _, err := io.ReadFull(r, b[1:2]); err != nil {
			return nil, err
		}
		domainLength := uint16(b[1])
		_, err := io.ReadFull(r, b[2:2+domainLength+2])
		return b[:1+1+domainLength+2], err
	case AtypIPv4:
		_, err := io.ReadFull(r, b[1:1+net.IPv4len+2])
		return b[:1+net.IPv4len+2], err
	case AtypIPv6:
		_, err := io.ReadFull(r, b[1:1+net.IPv6len+2])
		return b[:1+net.IPv6len+2], err
	}
	return nil, ErrAddressNotSupported
}

func SplitAddr(b []byte) Addr {
	addrLen := 1
	if len(b) < addrLen {
		return nil
	}
	switch b[0] {
	case AtypDomainName:
		if len(b) < 2 {
			return nil
		}
		addrLen = 1 + 1 + int(b[1]) + 2
	case AtypIPv4:
		addrLen = 1 + net.IPv4len + 2
	case AtypIPv6:
		addrLen = 1 + net.IPv6len + 2
	default:
		return nil
	}
	if len(b) < addrLen {
		return nil
	}
	return b[:addrLen]
}

func ParseAddr(s string) Addr {
	var addr Addr
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			addr = make([]byte, 1+net.IPv4len+2)
			addr[0] = AtypIPv4
			copy(addr[1:], ip4)
		} else {
			addr = make([]byte, 1+net.IPv6len+2)
			addr[0] = AtypIPv6
			copy(addr[1:], ip)
		}
	} else {
		if len(host) > 255 {
			return nil
		}
		addr = make([]byte, 1+1+len(host)+2)
		addr[0] = AtypDomainName
		addr[1] = byte(len(host))
		copy(addr[2:], host)
	}
	portnum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil
	}
	addr[len(addr)-2], addr[len(addr)-1] = byte(portnum>>8), byte(portnum)
	return addr
}

func ParseAddrToSocksAddr(addr net.Addr) Addr {
	var hostip net.IP
	var port int
	switch a := addr.(type) {
	case *net.UDPAddr:
		hostip, port = a.IP, a.Port
	case *net.TCPAddr:
		hostip, port = a.IP, a.Port
	}
	if hostip == nil {
		return ParseAddr(addr.String())
	}
	var parsed Addr
	if ip4 := hostip.To4(); ip4 != nil && ip4.DefaultMask() != nil {
		parsed = make([]byte, 1+net.IPv4len+2)
		parsed[0] = AtypIPv4
		copy(parsed[1:], ip4)
		binary.BigEndian.PutUint16(parsed[1+net.IPv4len:], uint16(port))
	} else {
		parsed = make([]byte, 1+net.IPv6len+2)
		parsed[0] = AtypIPv6
		copy(parsed[1:], hostip)
		binary.BigEndian.PutUint16(parsed[1+net.IPv6len:], uint16(port))
	}
	return parsed
}

// DecodeUDPPacket peels the SOCKS5 UDP header off `packet`, returning the
// embedded destination address and the payload slice.
func DecodeUDPPacket(packet []byte) (addr Addr, payload []byte, err error) {
	if len(packet) < 5 {
		err = errors.New("insufficient length of packet")
		return
	}
	if !bytes.Equal(packet[:2], []byte{0, 0}) {
		err = errors.New("reserved fields should be zero")
		return
	}
	if packet[2] != 0 {
		err = errors.New("discarding fragmented payload")
		return
	}
	addr = SplitAddr(packet[3:])
	if addr == nil {
		err = errors.New("failed to read UDP header")
		return
	}
	payload = packet[3+len(addr):]
	return
}

// EncodeUDPPacket wraps payload with a SOCKS5 UDP header pointing at addr.
func EncodeUDPPacket(addr Addr, payload []byte) ([]byte, error) {
	if addr == nil {
		return nil, errors.New("address is invalid")
	}
	return bytes.Join([][]byte{{0, 0, 0}, addr, payload}, nil), nil
}
