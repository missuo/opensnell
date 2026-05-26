/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// TestQUICEnvelopeRoundTrip verifies our server's decoder accepts our
// client's encoder for the same PSK / inner / target.
func TestQUICEnvelopeRoundTrip(t *testing.T) {
	psk := []byte("test-psk-2026")
	host := "example.com"
	port := uint16(443)
	inner := make([]byte, 1280)
	if _, err := rand.Read(inner); err != nil {
		t.Fatal(err)
	}
	inner[0] = 0xc0 // long header bit set, so it at least looks QUIC-ish

	env, err := EncodeQUICEnvelope(psk, host, port, inner)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	s := &Server{psk: psk}
	target, gotInner, _, err := s.decodeQUICEnvelope(env, "")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if target != "example.com:443" {
		t.Fatalf("target mismatch: got %q want example.com:443", target)
	}
	if !bytes.Equal(gotInner, inner) {
		t.Fatalf("inner mismatch: got %dB want %dB (first byte %02x vs %02x)",
			len(gotInner), len(inner), gotInner[0], inner[0])
	}
}

// TestQUICEnvelopeWithRealServerCapture verifies our decoder against an
// actual envelope packet captured from the live Surge snell-server v5.0.1.
// The byte stream is the literal UDP payload of the first 1359-byte
// packet that Surge sent to the server when establishing a QUIC tunnel
// to www.cloudflare.com:443. We expect to recover that target and an
// inner QUIC packet starting with the long-header byte 0xcd.
func TestQUICEnvelopeWithRealServerCapture(t *testing.T) {
	// PSK from the real Surge configuration used to generate this capture.
	psk := []byte("JFdZLmZLtYtUP308QrqdoA==")
	pkt := realCapturedEnvelopePkt1

	s := &Server{psk: psk}
	target, inner, _, err := s.decodeQUICEnvelope(pkt, "")
	if err != nil {
		t.Fatalf("decode real capture: %v", err)
	}
	if target != "www.cloudflare.com:443" {
		t.Fatalf("target = %q want www.cloudflare.com:443", target)
	}
	if len(inner) != 1280 {
		t.Fatalf("inner len = %d want 1280", len(inner))
	}
	if inner[0]&0xf0 != 0xc0 {
		t.Fatalf("inner first byte = 0x%02x, doesn't look like QUIC long header", inner[0])
	}
	// QUIC version 1 in bytes 1..4
	if !bytes.Equal(inner[1:5], []byte{0, 0, 0, 1}) {
		t.Fatalf("inner version bytes = % x, want 00 00 00 01", inner[1:5])
	}
}
