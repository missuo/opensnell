# Snell v6 — status

**Complete, validated against the official server, and closed-source.**

Snell v6 (the protocol behind Surge's `snell-server v6.0.0b1` / `v6.0.0b2` / `v6.0.0b3` / `v6.0.0b4`) has been
**fully reverse-engineered and reimplemented in Go** — client and server, end to end.
The implementation is **byte-for-byte identical to the official server on the wire**,
and we have decided **not to open-source it**.

## What v6 changed

Snell v6's headline feature is *PSK-derived, deployment-level protocol diversity*: every
deployment derives a unique "protocol profile" from its PSK — a salt scatter/mask, a
per-frame header-AAD sizing, and a per-frame **traffic-shaping** layer (padding plus a
padding↔ciphertext interleave) on top of the otherwise-unchanged v4/v5 AES-128-GCM core.
`v6.0.0b2` went further: it swapped the profile PRF and rebuilt the official server for
multi-core throughput and static linking. `v6.0.0b3` adds a `mode` directive that selects
how much of the stack is applied — `default` (obfs + AES), `unshaped` (AES only, no
shaping; ~10% more throughput, wire looks fully random like v3), and `unsafe-raw`
(plaintext, no crypto — trusted tunnels only). `v6.0.0b4` is a binary-only release —
**wire-identical to b3** (dependency refresh, an internal UDP-forwarding fix, and the
UPX self-extractor removed so it now ships unpacked, ~2.85 MB, still statically linked);
OpenSnell needed no protocol changes and now reports `v6.0.0b4`.

## What we built (and validated)

- A from-scratch **Go client and server** that derive the *entire* v6 profile from the
  PSK alone — no per-server probing — for **both b1 and b2**.
- **Byte-exact emission.** Against the unmodified official `snell-server v6.0.0b2`, our
  per-frame padding length matches the server's on **every frame** — 100% across 24 PSKs
  spanning every swap-mode and every write-mode, for small and large frames alike — and
  full HTTP fetches plus multi-hundred-KB echoes interoperate for **24 / 24** PSKs. On the
  wire, our traffic is indistinguishable from the official server's.
- **Performance on par with the hand-tuned C server.** The frame hot path is
  allocation-free (each frame is built in one reused per-connection buffer with in-place
  AEAD sealing), so at the same throughput the Go server's per-byte CPU matches the
  official `v6.0.0b2` server, with lower memory.

## Open-source policy

The v6 implementation is **closed-source** and will not be published. v6's entire purpose
is anti-fingerprinting, and keeping a byte-exact emitter unpublished is consistent with
that goal.

This public repository stays **GPLv3** and continues to provide:

- the full **Snell v4 / v5** server + client implementation, verified interoperable with
  the official `snell-server v5.0.1`; and
- an installer ([`install.sh`](install.sh)) that deploys the **official** Surge
  `snell-server v6.0.0b4` for anyone who wants to run v6 today.
