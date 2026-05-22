/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package utils

import (
	"io"
	"net"
	"time"

	p "github.com/missuo/opensnell/components/utils/pool"
)

// Relay bidirectionally copies between left and right until either side
// returns. The first side to finish forces the other to stop by setting an
// immediate read deadline.
func Relay(left, right net.Conn) (el, er error) {
	ch := make(chan error, 2)

	go func() {
		buf := p.Get(p.RelayBufferSize)
		_, err := io.CopyBuffer(left, right, buf)
		_ = p.Put(buf)
		_ = left.SetReadDeadline(time.Now())
		ch <- err
	}()

	buf := p.Get(p.RelayBufferSize)
	_, el = io.CopyBuffer(right, left, buf)
	_ = p.Put(buf)
	_ = right.SetReadDeadline(time.Now())
	er = <-ch

	if err, ok := el.(net.Error); ok && err.Timeout() {
		el = nil
	}
	if err, ok := er.(net.Error); ok && err.Timeout() {
		er = nil
	}
	return
}
