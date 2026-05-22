/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build !linux

package snell

import (
	"errors"
	"net"
	"runtime"
)

// On non-Linux platforms the brutal kernel module obviously cannot be
// installed; the helper returns a clear error so the calling layer can
// log a warning and continue with the OS-default congestion control.

func applyBrutal(_ net.Conn, _ uint64, _ uint32) error {
	return errors.New("tcp-brutal: not supported on " + runtime.GOOS + " (Linux kernel module only)")
}

func brutalSupported() bool { return false }
