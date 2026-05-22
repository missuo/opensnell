/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package obfs

import (
	"fmt"
	"net"

	"github.com/missuo/opensnell/components/obfs/http"
	"github.com/missuo/opensnell/components/obfs/tls"
)

func NewObfsServer(conn net.Conn, mode string) (net.Conn, error) {
	switch mode {
	case "tls":
		return tls.NewTLSObfsServer(conn), nil
	case "http":
		return http.NewHTTPObfsServer(conn), nil
	case "none", "off", "":
		return conn, nil
	default:
		return nil, fmt.Errorf("invalid obfs type %s", mode)
	}
}

func NewObfsClient(conn net.Conn, server, port, mode string) (net.Conn, error) {
	switch mode {
	case "tls":
		return tls.NewTLSObfsClient(conn, server), nil
	case "http":
		return http.NewHTTPObfsClient(conn, server, port), nil
	case "none", "off", "":
		return conn, nil
	default:
		return nil, fmt.Errorf("invalid obfs type %s", mode)
	}
}
