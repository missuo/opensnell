/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build !linux && !darwin

package tun

import (
	"context"
	"log/slog"
)

// New returns ErrUnsupported on non-linux platforms. The same binary
// still compiles everywhere; users who don't pass --tun never reach
// this code path.
func New(_ context.Context, _ Config, _ Dialer, _ *slog.Logger) (Inbound, error) {
	return nil, ErrUnsupported
}
