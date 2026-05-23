/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build darwin

package tun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// darwinBypass implements BypassManager for macOS by installing
// host- or net-scope routes pinned to the host's default physical
// interface (typically en0 / en1). Because sing-tun's AutoRoute
// installs sub-prefix routes covering 0.0.0.0/0 over the utun, a
// more-specific /32 or /128 via en0 wins longest-prefix-match and
// the kernel forwards those packets via the real interface instead
// of into our userspace stack.
//
// State file: every successful route insert is also written to
// bypassStateFile (atomic temp+rename). On startup, the file is
// replayed in reverse — each entry gets `route delete`d before we
// truncate the file. This recovers from SIGKILL/panic in the
// previous run, which would otherwise leave the kernel routing
// table polluted with /32 entries no one owns anymore.
//
// CLI-only. No launchd helper. The whole snell-client process runs
// under sudo so it can call `route` directly. A future GUI app
// would slot in a helper-IPC implementation of the same
// BypassManager interface — the consumer code (tun_darwin.go,
// forward.go) doesn't change.
type darwinBypass struct {
	log       *slog.Logger
	iface     string // default physical interface, e.g. "en0"
	stateFile string

	mu          sync.Mutex
	staticCIDRs []netip.Prefix
	dynamicIPs  map[netip.Addr]time.Time

	reaperOnce   sync.Once
	reaperCancel context.CancelFunc
	reaperWG     sync.WaitGroup
}

// bypassStateFile lives under /var/run so it's automatically wiped
// on reboot — no point keeping the list across boot anyway, the
// kernel routing table is also empty then. /var/run is writable as
// root which we always are.
const bypassStateFile = "/var/run/opensnell-bypass.state"

// newDarwinBypass detects the current default interface, kicks off
// a stale-route reclaim from any previous (potentially SIGKILLed)
// run, and starts the dynamic-IP TTL reaper. iface override is
// honored when non-empty (mostly for tests); otherwise the value
// comes from `route -n get default`.
func newDarwinBypass(ctx context.Context, ifaceOverride string, log *slog.Logger) (*darwinBypass, error) {
	iface := ifaceOverride
	if iface == "" {
		detected, err := detectDefaultInterface()
		if err != nil {
			return nil, fmt.Errorf("bypass darwin: detect default interface: %w", err)
		}
		iface = detected
	}
	b := &darwinBypass{
		log:        log,
		iface:      iface,
		stateFile:  bypassStateFile,
		dynamicIPs: make(map[netip.Addr]time.Time),
	}
	b.reclaim()

	reaperCtx, cancel := context.WithCancel(ctx)
	b.reaperCancel = cancel
	b.reaperWG.Add(1)
	go b.reaperLoop(reaperCtx)

	log.Info("bypass darwin ready", "iface", iface, "state-file", b.stateFile)
	return b, nil
}

func (b *darwinBypass) AddCIDR(p netip.Prefix) error {
	if !p.IsValid() {
		return nil
	}
	if !p.Addr().Is4() {
		// IPv6 direct routing is a no-op today: macOS auto-route on
		// our darwin TUN only covers IPv4, so v6 traffic already
		// flows through the host's real interface unchanged. We
		// silently accept the call so the cross-platform
		// DirectIPs/Direct Domain wiring stays clean.
		b.log.Debug("bypass darwin: skip v6 cidr", "prefix", p)
		return nil
	}
	if err := routeAdd(p, b.iface); err != nil {
		// Already-exists is non-fatal — happens after a reload
		// when reclaim already handled the old entries but raced
		// against a quick restart.
		if !isRouteExistsError(err) {
			return fmt.Errorf("route add %s: %w", p, err)
		}
		b.log.Debug("bypass darwin: route already exists", "prefix", p)
	}
	b.mu.Lock()
	b.staticCIDRs = append(b.staticCIDRs, p)
	b.persistLocked()
	b.mu.Unlock()
	return nil
}

func (b *darwinBypass) AddIP(ip netip.Addr, ttl time.Duration) error {
	if !ip.IsValid() {
		return nil
	}
	ip = ip.Unmap()
	if !ip.Is4() {
		// See AddCIDR — v6 is no-op on macOS.
		b.log.Debug("bypass darwin: skip v6 ip", "ip", ip)
		return nil
	}
	if ttl <= 0 {
		// Permanent — equivalent to a /32 static CIDR.
		bits := 32
		return b.AddCIDR(netip.PrefixFrom(ip, bits))
	}
	b.mu.Lock()
	newExpiry := time.Now().Add(ttl)
	prev, existed := b.dynamicIPs[ip]
	if existed && prev.After(newExpiry) {
		b.mu.Unlock()
		return nil // existing entry already lives longer, nothing to do.
	}
	b.dynamicIPs[ip] = newExpiry
	if !existed {
		b.mu.Unlock()
		if err := routeAdd(netip.PrefixFrom(ip, 32), b.iface); err != nil && !isRouteExistsError(err) {
			// Roll back map entry on hard failure so future TTL
			// refreshes can retry.
			b.mu.Lock()
			delete(b.dynamicIPs, ip)
			b.mu.Unlock()
			return fmt.Errorf("route add %s: %w", ip, err)
		}
		b.mu.Lock()
		b.persistLocked()
		b.mu.Unlock()
		return nil
	}
	b.persistLocked()
	b.mu.Unlock()
	return nil
}

func (b *darwinBypass) Close() error {
	b.reaperOnce.Do(func() {
		if b.reaperCancel != nil {
			b.reaperCancel()
		}
		b.reaperWG.Wait()

		b.mu.Lock()
		statics := append([]netip.Prefix(nil), b.staticCIDRs...)
		dynamic := make([]netip.Addr, 0, len(b.dynamicIPs))
		for ip := range b.dynamicIPs {
			dynamic = append(dynamic, ip)
		}
		b.staticCIDRs = nil
		b.dynamicIPs = map[netip.Addr]time.Time{}
		b.persistLocked()
		b.mu.Unlock()

		for _, p := range statics {
			if err := routeDelete(p); err != nil && !isRouteMissingError(err) {
				b.log.Debug("bypass darwin: close route delete", "prefix", p, "err", err)
			}
		}
		for _, ip := range dynamic {
			if err := routeDelete(netip.PrefixFrom(ip, 32)); err != nil && !isRouteMissingError(err) {
				b.log.Debug("bypass darwin: close route delete", "ip", ip, "err", err)
			}
		}
	})
	return nil
}

// reaperLoop sweeps expired dynamic IPs every reaperInterval and
// issues `route delete` for each. Failure to delete is logged but
// not fatal — the next Close or next startup reclaim will retry.
func (b *darwinBypass) reaperLoop(ctx context.Context) {
	defer b.reaperWG.Done()
	t := time.NewTicker(reaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.sweepExpired()
		}
	}
}

func (b *darwinBypass) sweepExpired() {
	now := time.Now()
	var expired []netip.Addr
	b.mu.Lock()
	for ip, exp := range b.dynamicIPs {
		if exp.Before(now) {
			expired = append(expired, ip)
			delete(b.dynamicIPs, ip)
		}
	}
	if len(expired) > 0 {
		b.persistLocked()
	}
	b.mu.Unlock()
	for _, ip := range expired {
		if err := routeDelete(netip.PrefixFrom(ip, 32)); err != nil && !isRouteMissingError(err) {
			b.log.Debug("bypass darwin: reaper route delete", "ip", ip, "err", err)
		} else {
			b.log.Debug("bypass darwin: reaped", "ip", ip)
		}
	}
}

// persistLocked writes the current authoritative set to stateFile
// using atomic temp+rename. Caller must hold b.mu.
func (b *darwinBypass) persistLocked() {
	var sb strings.Builder
	for _, p := range b.staticCIDRs {
		sb.WriteString(p.String())
		sb.WriteByte('\n')
	}
	for ip := range b.dynamicIPs {
		sb.WriteString(netip.PrefixFrom(ip, 32).String())
		sb.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(b.stateFile), 0o755); err != nil {
		b.log.Debug("bypass darwin: state dir mkdir", "err", err)
		return
	}
	tmp := b.stateFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o644); err != nil {
		b.log.Debug("bypass darwin: state write", "err", err)
		return
	}
	if err := os.Rename(tmp, b.stateFile); err != nil {
		b.log.Debug("bypass darwin: state rename", "err", err)
	}
}

// reclaim is called once at startup. It reads stateFile (entries
// left behind by a previous, ungracefully-killed run) and runs
// `route delete` on each. Errors are logged at debug — a missing
// route is normal (the kernel may have already evicted it on
// interface change). The file is truncated afterward so the next
// startup doesn't re-attempt the same deletes.
func (b *darwinBypass) reclaim() {
	f, err := os.Open(b.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			b.log.Debug("bypass darwin: open state file", "err", err)
		}
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var reclaimed int
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		p, err := netip.ParsePrefix(line)
		if err != nil {
			b.log.Debug("bypass darwin: state parse skip", "line", line, "err", err)
			continue
		}
		if err := routeDelete(p); err != nil && !isRouteMissingError(err) {
			b.log.Debug("bypass darwin: reclaim delete failed", "prefix", p, "err", err)
			continue
		}
		reclaimed++
	}
	if reclaimed > 0 {
		b.log.Info("bypass darwin: reclaimed stale routes", "count", reclaimed)
	}
	// Truncate regardless of per-line outcomes — anything we
	// couldn't delete is either gone already or someone else's
	// problem; either way we no longer claim it.
	_ = os.WriteFile(b.stateFile, nil, 0o644)
}

// --- route command helpers ---

// routeAdd installs a host- or net-route via the given interface.
// Always specifies -interface to override sing-tun's auto-route
// catch-all; the kernel picks the more-specific /32 over the
// half-prefix utun route.
func routeAdd(p netip.Prefix, iface string) error {
	args := buildRouteArgs("add", p, iface)
	cmd := exec.Command("route", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return wrapRouteErr(args, out, err)
	}
	return nil
}

// routeDelete removes a route. No -interface needed for delete on
// macOS — the kernel locates by destination.
func routeDelete(p netip.Prefix) error {
	args := []string{"delete"}
	if p.Addr().Is6() {
		args = append(args, "-inet6")
	}
	if p.IsSingleIP() {
		args = append(args, "-host", p.Addr().String())
	} else {
		args = append(args, "-net", p.String())
	}
	cmd := exec.Command("route", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return wrapRouteErr(args, out, err)
	}
	return nil
}

func buildRouteArgs(verb string, p netip.Prefix, iface string) []string {
	args := []string{verb}
	if p.Addr().Is6() {
		args = append(args, "-inet6")
	}
	if p.IsSingleIP() {
		args = append(args, "-host", p.Addr().String())
	} else {
		args = append(args, "-net", p.String())
	}
	if iface != "" {
		args = append(args, "-interface", iface)
	}
	return args
}

func wrapRouteErr(args []string, out []byte, err error) error {
	return fmt.Errorf("route %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

func isRouteExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "file exists") || strings.Contains(msg, "already in table")
}

func isRouteMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not in table") || strings.Contains(msg, "no such process")
}

// detectDefaultInterface parses `route -n get default` to find the
// host's outgoing physical interface. We pin direct routes there
// so the kernel's longest-prefix-match wins against sing-tun's
// auto-route half-prefixes installed on the utun.
//
// Detected once at startup. If the user switches Wi-Fi → Ethernet
// at runtime, direct routes still point at the old interface and
// stop working until snell-client is restarted. A future enhancement
// could subscribe to sing-tun's default-interface monitor and
// reinstall on change; punted as a known limitation.
func detectDefaultInterface() (string, error) {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return "", fmt.Errorf("route get default: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			iface := strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
			if iface == "" {
				return "", errors.New("default interface is empty")
			}
			return iface, nil
		}
	}
	return "", errors.New("no default interface in route output")
}
