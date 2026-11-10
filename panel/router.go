package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- Config ---

var (
	routerIP         = getenv("ROUTER_IP", "192.168.31.1")
	routerPass       = getenv("ROUTER_ROOT_PASSWORD", "root")
	ntfyTopicEnv     = getenv("NTFY_TOPIC", "")
	notifyBackendEnv = getenv("NOTIFY_BACKEND", "ntfy")
	telegramTokenEnv = getenv("TELEGRAM_BOT_TOKEN", "")
	telegramChatEnv  = getenv("TELEGRAM_CHAT_ID", "")
	keyPath          string // set in main(), next to the executable
	envFilePath      string // set in main(), the .env next to the executable
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	sshCtrlDirOnce sync.Once
	sshCtrlDir     string
)

// sshControlDir picks a short, writable directory for the SSH multiplexing
// control socket. It prefers /tmp — short and present on macOS and Linux, which
// keeps the socket path under the ~104-char unix-domain-socket limit that
// os.TempDir() on macOS (/var/folders/.../T, plus OpenSSH's random master
// suffix) blows past. It falls back to os.TempDir() only where /tmp isn't a
// writable directory (e.g. cpe-box running on Android under KSWEB, where there
// is no /tmp but TMPDIR points somewhere usable). Computed once.
func sshControlDir() string {
	sshCtrlDirOnce.Do(func() {
		for _, d := range []string{"/tmp", os.TempDir()} {
			if d == "" {
				continue
			}
			if f, err := os.CreateTemp(d, "cpebox_probe"); err == nil {
				name := f.Name()
				f.Close()
				os.Remove(name)
				sshCtrlDir = strings.TrimRight(d, "/")
				return
			}
		}
		sshCtrlDir = strings.TrimRight(os.TempDir(), "/")
	})
	return sshCtrlDir
}

func sshOpts() []string {
	opts := []string{
		"-o", "StrictHostKeyChecking=no",
		// This router regenerates its dropbear host key on every boot (same
		// ramfs /etc as everything else that doesn't persist) - checking it
		// would just mean every reboot breaks the connection with "REMOTE
		// HOST IDENTIFICATION HAS CHANGED", which also makes OpenSSH refuse
		// password auth outright even with StrictHostKeyChecking=no. There's
		// no host identity here worth verifying against, so don't keep one.
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "HostKeyAlgorithms=+ssh-rsa",
		"-o", "PubkeyAcceptedAlgorithms=+ssh-rsa",
		"-o", "ConnectTimeout=5",
	}
	// Connection multiplexing uses a Unix-domain control socket, which Windows
	// OpenSSH doesn't support: it fails every command with "getsockname failed:
	// Not a socket". So only enable it off Windows; Windows opens a fresh
	// connection per call instead.
	//
	// Where it is used: the router's dropbear is happy to open a new SSH
	// session on an existing connection but gets angry ("kex_exchange_
	// identification: Connection reset by peer") when several handlers each try
	// to open a fresh connection at the same time. ControlMaster=auto keeps one
	// connection warm for 60s so subsequent calls piggy-back on it. The socket
	// name uses %h (the router IP) rather than %C (a 40-char hash) because a
	// unix-domain path has a ~104-char limit and OpenSSH appends a ~17-char
	// random suffix while creating the master.
	if runtime.GOOS != "windows" {
		ctrlPath := sshControlDir() + "/cpebox_%h"
		opts = append(opts,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+ctrlPath,
			"-o", "ControlPersist=60",
		)
	}
	return opts
}

// RouterError is returned for anything that should be shown to the user as
// a clean error message rather than a Go-internal one.
type RouterError struct{ msg string }

func (e *RouterError) Error() string { return e.msg }

func routerErrf(format string, a ...any) error {
	return &RouterError{fmt.Sprintf(format, a...)}
}

// -------------------------------------------------------------- SSH exec ---

// runSSHRaw runs one command on the router over SSH (key-based or
// password-based) and reports back exactly what happened, without
// interpreting it - run()/runBg() decide what any of it means.
func runSSHRaw(useKey bool, cmd string, timeout time.Duration) (stdout, stderr string, exitCode int, timedOut, notFound bool, err error) {
	var name string
	var args []string
	// Password-auth path doesn't shell out any more: use the Go SSH client
	// directly, which (1) removes the sshpass dependency (Windows and bare
	// Linux containers don't ship it), (2) keeps the password out of the
	// process arg list, and (3) is identical across platforms.
	if !useKey {
		return runSSHPassword(cmd, timeout)
	}

	name = "ssh"
	// BatchMode: never fall back to an interactive password prompt on the
	// key attempt. Without it a rejected key (or a stuck control socket)
	// leaves ssh waiting on "root@host's password:" until our deadline,
	// surfacing as a confusing "Timed out running" instead of a fast 255
	// that run() can retry with the password.
	args = append([]string{"-i", keyPath, "-o", "BatchMode=yes"}, sshOpts()...)
	args = append(args, "root@"+routerIP, cmd)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	runErr := c.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out.String(), errb.String(), -1, true, false, nil
	}
	if runErr != nil {
		var execErr *exec.Error
		if errors.As(runErr, &execErr) {
			// e.g. sshpass isn't installed - distinct from a normal
			// nonzero exit, since the command never actually ran.
			return "", "", -1, false, true, runErr
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return out.String(), errb.String(), exitErr.ExitCode(), false, false, nil
		}
		return out.String(), errb.String(), -1, false, false, runErr
	}
	return out.String(), errb.String(), 0, false, false, nil
}

// reinstallKey re-adds our public key to authorized_keys - used after the
// key stops working (e.g. the router rebooted and dropbear came back up
// with a fresh /etc, which does happen on this platform).
func reinstallKey() {
	pubBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return
	}
	pubkey := strings.TrimSpace(string(pubBytes))
	cmd := fmt.Sprintf(
		`mkdir -p /etc/dropbear; grep -qF "%s" /etc/dropbear/authorized_keys 2>/dev/null || echo "%s" >> /etc/dropbear/authorized_keys; chmod 600 /etc/dropbear/authorized_keys`,
		pubkey, pubkey,
	)
	// Self-healing is best-effort: it must never fail the original request,
	// so any error here (including a missing sshpass binary) is swallowed.
	_, _, _, _, _, _ = runSSHRaw(false, cmd, 10*time.Second)
}

// run executes a single shell command on the router over SSH and returns
// its stdout.
//
// Tries the key first (fast, the normal path). If that fails (typically
// because the router rebooted and dropbear came back up without our key in
// authorized_keys), falls back to the persistent root password (unlike the
// key, this one is confirmed to survive a reboot) and quietly reinstalls
// the key so the next call goes through the fast path again without any
// manual intervention.
func run(cmd string, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	stdout, stderr, code, timedOut, _, err := runSSHRaw(true, cmd, timeout)
	if err != nil {
		return "", routerErrf("SSH error running %q: %v", cmd, err)
	}
	if timedOut {
		return "", routerErrf("Timed out running: %s", cmd)
	}

	keyAuthFailed := code == 255 && (strings.Contains(stderr, "Permission denied") || strings.Contains(stderr, "Connection closed"))
	if keyAuthFailed {
		stdout2, stderr2, code2, timedOut2, notFound2, err2 := runSSHRaw(false, cmd, timeout)
		if notFound2 {
			return "", routerErrf(
				"SSH key login failed and no router password is on file to retry with (%v). "+
					"Re-run setup to reinstall the SSH key, or set ROUTER_ROOT_PASSWORD in panel/.env.", err2)
		}
		if err2 != nil {
			return "", routerErrf("SSH error running %q (password fallback): %v", cmd, err2)
		}
		if timedOut2 {
			return "", routerErrf("Timed out running (password fallback): %s", cmd)
		}
		stdout, stderr, code = stdout2, stderr2, code2
		if code == 0 || code == 1 {
			reinstallKey() // fix the key for next time, since we got in via password anyway
		}
	}

	if code != 0 && code != 1 { // 1 is often just "grep found nothing", not fatal
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(stdout)
		}
		return "", routerErrf("Error (%d): %s", code, msg)
	}
	return stdout, nil
}

// runBg executes a potentially slow command (e.g. a wifi reload) and
// tolerates it running long, rather than treating that as an error.
func runBg(cmd string, timeout time.Duration) string {
	if timeout == 0 {
		timeout = 150 * time.Second
	}
	stdout, stderr, code, timedOut, _, err := runSSHRaw(true, cmd, timeout)
	if timedOut {
		return "(command is still running on the router, the interface should come back up on its own within 1-2 minutes)"
	}
	if err == nil && code == 255 && strings.Contains(stderr, "Permission denied") {
		stdout2, stderr2, code2, timedOut2, notFound2, err2 := runSSHRaw(false, cmd, timeout)
		if timedOut2 {
			return "(command is still running on the router, the interface should come back up on its own within 1-2 minutes)"
		}
		if !notFound2 && err2 == nil {
			stdout, stderr, code = stdout2, stderr2, code2
			if code == 0 || code == 1 {
				reinstallKey()
			}
		}
	}
	return stdout + stderr
}

// ---------------------------------------------------------------- Cellular ---

const atPort = "/dev/ttyUSB2"

var atCommandQuoteRe = regexp.MustCompile("'")

// atMutex serializes AT queries from this process; the lock microcom
// takes (below) additionally keeps them from interleaving with the stock
// mobile daemon, which polls the same port.
var atMutex sync.Mutex

// atQuery sends AT commands through busybox microcom - exactly how the
// stock firmware's own /usr/sbin/at_cmd.sh talks to the modem. microcom
// holds /var/lock/LCK..ttyUSB2 while it runs, and the stock mobile daemon
// polls this same port (AT+QCAINFO every few seconds) via that script, so
// going through the lock is what keeps the two from garbling each other's
// replies. Writing straight to the tty with a background `cat` reader, as
// this used to, raced with the daemon and produced the intermittent
// empty/partial readbacks. microcom never exits by itself while the port
// stays busy, so it's killed once the replies have had time to arrive.
func atQuery(commands []string, wait time.Duration) (string, error) {
	atMutex.Lock()
	defer atMutex.Unlock()
	var feed []string
	for _, c := range commands {
		if atCommandQuoteRe.MatchString(c) {
			return "", fmt.Errorf("AT command can't contain a single quote: %q", c)
		}
		feed = append(feed, fmt.Sprintf(`printf '%s\r'; usleep %d`, c, wait.Microseconds()))
	}
	total := time.Duration(len(commands))*wait + 500*time.Millisecond
	script := fmt.Sprintf(`L=/var/lock/LCK..ttyUSB2; O=/tmp/cpebox_at.$$
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  [ -e $L ] || break
  OWNER=$(tr -d ' \n' < $L 2>/dev/null)
  [ -n "$OWNER" ] && ! kill -0 "$OWNER" 2>/dev/null && rm -f $L && break
  usleep 250000
done
( %s ) | busybox microcom %s > $O 2>&1 & P=$!
usleep %d
kill $P 2>/dev/null; wait $P 2>/dev/null
cat $O; rm -f $O`, strings.Join(feed, "; "), atPort, total.Microseconds())
	var out string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		out, err = run(script, 20*time.Second+total)
		if err != nil {
			return "", err
		}
		if !strings.Contains(out, "can't create") {
			return out, nil
		}
		time.Sleep(time.Second)
	}
	return "", routerErrf("Modem AT port stayed busy, try again")
}

var qnwprefcfgRe = regexp.MustCompile(`\+QNWPREFCFG:\s*"([a-zA-Z0-9_]+)",\s*([^\r\n]+)`)

func parseQnwprefcfg(raw string) map[string]string {
	out := map[string]string{}
	for _, m := range qnwprefcfgRe.FindAllStringSubmatch(raw, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

// modemBandSupport lists the bands a given modem model can actually use,
// keyed by the prefix of its firmware version string. Measured on the
// device itself: writing a superset of every band to the modem and reading
// back ue_capability_band, which the modem reports as (configured bands) ∩
// (bands the hardware supports).
var modemBandSupport = map[string]struct{ lte, nr string }{
	"RG520NEB": {"1,3,7,8,20,28,32,38,42,43", "1,3,7,8,20,28,38,75,76,77,78"},
}

func commaToColon(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), ",", ":") }

func readUciBands() (map[string]string, error) {
	raw, err := run(`echo "LTE=$(uci -q get mobile.device.lte_band)"; echo "SA=$(uci -q get mobile.device.sa_band)"; echo "NSA=$(uci -q get mobile.device.nsa_band)"; echo "MODEL=$(uci -q get mobile.device.version)"`, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return parseConf(raw), nil
}

// getModemConfig returns the band configuration the way the stock mobile
// daemon holds it (UCI mobile.device.*, persisted in /data/etc/config/mobile).
// That is the real setting: the daemon re-programs the modem from it every
// time it starts, including on every boot, overriding anything written to
// the modem directly. Alongside it: what the modem effectively advertises
// right now (ue_capability_band = configured ∩ hardware-supported), the
// SA/NSA mode, and the model's supported bands when known.
func getModemConfig() (map[string]string, error) {
	u, err := readUciBands()
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{
		"lte_band":      commaToColon(u["LTE"]),
		"nr5g_band":     commaToColon(u["SA"]),
		"nsa_nr5g_band": commaToColon(u["NSA"]),
		"modem_model":   u["MODEL"],
	}
	for prefix, sup := range modemBandSupport {
		if strings.HasPrefix(u["MODEL"], prefix) {
			cfg["hw_lte_band"] = commaToColon(sup.lte)
			cfg["hw_nr5g_band"] = commaToColon(sup.nr)
		}
	}
	if raw, err := atQuery([]string{`AT+QNWPREFCFG="ue_capability_band"`, `AT+QNWPREFCFG="nr5g_disable_mode"`, `AT+QNWPREFCFG="mode_pref"`}, 1200*time.Millisecond); err == nil {
		p := parseQnwprefcfg(raw)
		for _, k := range []string{"lte_band", "nr5g_band", "nsa_nr5g_band"} {
			if v, ok := p[k]; ok {
				cfg["effective_"+k] = v
			}
		}
		if v, ok := p["nr5g_disable_mode"]; ok {
			cfg["nr5g_disable_mode"] = panelMode(p["mode_pref"], v)
		}
	}
	return cfg, nil
}

var (
	// The tail after the band is captured too: an SCC line whose RSRP (two
	// fields after scell_state) is 0 or "-" is a configured but inactive
	// carrier, which the stock daemon leaves out of its band list as well:
	//   +QCAINFO: "SCC",431070,3,"NR5G BAND 1",1,450,0,-,-
	qcaBandRe = regexp.MustCompile(`\+QCAINFO:\s*"(PCC|SCC)",(\d+),[^"]*"(LTE|NR5G)\s+BAND\s+(\d+)"([^\r\n]*)`)
	// The primary carrier's ARFCN/EARFCN is the first number on the PCC line:
	//   +QCAINFO: "PCC",634080,12,"NR5G BAND 78",...
	qcaPccRe = regexp.MustCompile(`\+QCAINFO:\s*"PCC",(\d+),[^"]*"(LTE|NR5G)\s+BAND`)
)

// aggregatedBands runs AT+QCAINFO and returns the currently attached bands as
// "B3+B8+n1+n78", plus the primary carrier's ARFCN/EARFCN and its RAT
// ("LTE"/"NR5G"). Best-effort — empty strings on any hiccup, so the caller can
// fall back to the primary band/ARFCN from the daemon or QENG.
// readQCAINFO returns the raw AT+QCAINFO reply, or "" on any error. A var so
// tests can feed captured replies instead of reaching a live modem.
var readQCAINFO = func() string {
	// The reply is complete within ~300 ms; a short wait keeps the port free
	// for the console and the rest of the poll.
	raw, err := atQuery([]string{`AT+QCAINFO`}, 800*time.Millisecond)
	if err != nil {
		return ""
	}
	return raw
}

func aggregatedBands() (bands, pccArfcn, pccRat string) {
	raw := readQCAINFO()
	if raw == "" {
		return "", "", ""
	}
	if m := qcaPccRe.FindStringSubmatch(raw); m != nil && m[1] != "0" {
		pccArfcn, pccRat = m[1], m[2]
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range qcaBandRe.FindAllStringSubmatch(raw, -1) {
		arfcn, rat, band := m[2], m[3], m[4]
		if m[1] == "SCC" && qcaInactive(m[5]) {
			continue
		}
		prefix := "B"
		if rat == "NR5G" {
			prefix = "n"
		}
		if band == "0" {
			// No band 0 exists; cb0401 v1 firmware reports it as a placeholder
			// on some NR carriers while the ARFCN is still valid. Recover the
			// band from the ARFCN (as the stock UI does); if that fails, skip
			// it and let the caller fall back to QNWINFO/QENG rather than
			// surfacing a bogus "n0" in the Carriers row.
			if rat == "NR5G" {
				if b := nrBandFromArfcn(arfcn); b != 0 {
					band = strconv.Itoa(b)
				}
			}
			if band == "0" {
				continue
			}
		}
		key := prefix + band
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return strings.Join(out, "+"), pccArfcn, pccRat
}

// qcaInactive reports whether the fields after the band on an SCC line
// (",<scell_state>,<pci>,<rsrp>,...") show a carrier with no signal. A bare
// ",<pci>" (the NSA NR leg) is active.
func qcaInactive(tail string) bool {
	f := strings.Split(strings.TrimPrefix(strings.TrimSpace(tail), ","), ",")
	if len(f) < 3 {
		return false
	}
	rsrp := strings.TrimSpace(f[2])
	return rsrp == "0" || rsrp == "-"
}

// simNumber backfills the SIM's own number from AT+CNUM when the stock
// daemon leaves it blank (v2 firmware does, though the SIM has it). It
// doesn't change while the SIM is in, so the AT read is cached.
func simNumber(daemon string) string {
	if daemon != "" {
		return daemon
	}
	v, err := cached("simnumber", 10*time.Minute, func() (any, error) {
		raw, err := atQuery([]string{`AT+CNUM`}, 600*time.Millisecond)
		if err != nil {
			return nil, err
		}
		if m := cnumRe.FindStringSubmatch(raw); m != nil {
			return m[1], nil
		}
		return "", nil
	})
	if err != nil {
		return ""
	}
	n, _ := v.(string)
	return n
}

// nrArfcnBands maps NR-ARFCN (SSB/DL) ranges to their 3GPP band number
// (TS 38.104 Table 5.4.2.3-1, FR1). Ranges overlap between some bands, so the
// list is ordered to resolve an overlap toward the band this hardware is far
// more likely to be on (n1 before n66, n78 before n77, FDD before the wide
// TDD n41). Only used to recover a band the modem reports as 0.
var nrArfcnBands = []struct{ lo, hi, band int }{
	{422000, 434000, 1},
	{361000, 376000, 3},
	{173800, 178800, 5},
	{524000, 538000, 7},
	{185000, 192000, 8},
	{145800, 149200, 12},
	{151600, 160600, 28},
	{158200, 164200, 20},
	{376000, 384000, 39},
	{460000, 480000, 40},
	{514000, 524000, 38},
	{499200, 513999, 41},
	{123400, 130400, 71},
	{620000, 653333, 78},
	{653334, 680000, 77},
	{693334, 733333, 79},
	{399000, 404000, 70},
	{434000, 440000, 66},
}

// nrBandFromArfcn returns the NR band number for an NR-ARFCN, or 0 if none
// matches (bad input or an ARFCN outside the ranges above).
func nrBandFromArfcn(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0
	}
	for _, r := range nrArfcnBands {
		if n >= r.lo && n <= r.hi {
			return r.band
		}
	}
	return 0
}

// nrBandLabel formats a 5G band for display ("n1"). cb0401 v1 firmware
// (ROM 3.0.116) sometimes reports the NR band field as 0 on a live cell while
// the ARFCN is correct — the stock UI derives the band from the ARFCN, so do
// the same here rather than showing a bogus "n0".
func nrBandLabel(band, arfcn string) string {
	if band != "" && band != "0" {
		return "n" + band
	}
	if b := nrBandFromArfcn(arfcn); b != 0 {
		return "n" + strconv.Itoa(b)
	}
	if band != "" {
		return "n" + band
	}
	return ""
}

// getCellularInfo pulls the live serving-cell info from the router's stock
// modem daemon (`ubus call mobile dump_status`) — the same source the stock
// web UI uses, so the values match router.miwifi.com and the daemon does the
// work in one fast JSON call. The aggregated CA band list (which the daemon
// doesn't expose) is fetched via one AT+QCAINFO on top, best-effort.
//
// Older hardware/firmware (e.g. cb0401 v1, ROM 3.0.116) ships a mobile daemon
// that has no `dump_status` method at all — the call fails with ubus status 3
// (METHOD_NOT_FOUND), which used to leave the whole cellular card dead and the
// header stuck on "connecting…". When the daemon path is unavailable we fall
// back to reading the same information straight from the modem over AT
// (getCellularInfoViaAT), so those units get a working card too.
func getCellularInfo() (map[string]any, error) {
	raw, derr := run("ubus call mobile dump_status", 10*time.Second)
	if derr == nil {
		if info, perr := parseDumpStatus(raw); perr == nil {
			fillMissingSINR(info)
			fillMissingWAN(info)
			return info, nil
		}
	}
	// Daemon missing/without dump_status, or JSON we couldn't parse — rebuild
	// the essentials straight from the modem. Keep the daemon error so that,
	// if AT also fails, the message names both causes.
	info, aerr := getCellularInfoViaAT()
	if aerr != nil {
		if derr != nil {
			return nil, routerErrf("mobile daemon unavailable (%v) and AT fallback failed (%v)", derr, aerr)
		}
		return nil, aerr
	}
	fillMissingWAN(info)
	return info, nil
}

// cgpaddrRe matches one +CGPADDR row: cid, up to two quoted addresses.
var cgpaddrRe = regexp.MustCompile(`\+CGPADDR:\s*\d+,\s*"([^"]*)"(?:\s*,\s*"([^"]*)")?`)

// parseCGPADDR pulls the first non-zero IPv4 and IPv6 out of a reply block.
// The modem reports each PDP context as either an IPv4 ("A.B.C.D"), an IPv6
// written as 16 decimal octets ("42.1.5...71.3"), or both when IPV4V6.
func parseCGPADDR(raw string) (v4, v6 string) {
	for _, m := range cgpaddrRe.FindAllStringSubmatch(raw, -1) {
		for _, a := range []string{m[1], m[2]} {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			oct := strings.Split(a, ".")
			switch len(oct) {
			case 4:
				if v4 == "" && a != "0.0.0.0" {
					v4 = a
				}
			case 16:
				if ip := decodeV6Octets(oct); ip != "" && v6 == "" {
					v6 = ip
				}
			}
		}
	}
	return v4, v6
}

// decodeV6Octets turns 16 decimal octets ("42.1.5...71.3") into "2a01:...".
// Returns "" for the all-zero address.
func decodeV6Octets(oct []string) string {
	all0 := true
	buf := make([]byte, 16)
	for i, s := range oct {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 0 || n > 255 {
			return ""
		}
		if n != 0 {
			all0 = false
		}
		buf[i] = byte(n)
	}
	if all0 {
		return ""
	}
	ip := net.IP(buf).String() // uses the standard :: shortening
	return ip
}

// fillMissingWAN backfills the operator-assigned WAN addresses from AT+CGPADDR
// when dump_status didn't carry them (seen on cb0401 v1, issue #3) or when the
// AT fallback path built the info - CGPADDR is the modem's own view of what the
// network handed out and works regardless of the stock daemon's shape.
func fillMissingWAN(info map[string]any) {
	needV4 := info["wan_v4"] == nil || info["wan_v4"] == "" || info["wan_v4"] == "0.0.0.0"
	needV6 := info["wan_v6"] == nil || info["wan_v6"] == ""
	if !needV4 && !needV6 {
		return
	}
	raw, err := atQuery([]string{"AT+CGPADDR"}, 600*time.Millisecond)
	if err != nil {
		return
	}
	v4, v6 := parseCGPADDR(raw)
	if needV4 && v4 != "" {
		info["wan_v4"] = v4
	}
	if needV6 && v6 != "" {
		info["wan_v6"] = v6
	}
}

// fillMissingSINR backfills SINR from the modem's own AT+QENG when the stock
// daemon's dump_status left it blank or reported a bare 0 for a leg that's
// actually connected. Some firmware (seen on cb0401 v2 on an LTE-only cell)
// simply doesn't populate the LTE SINR in dump_status, leaving the Signal card
// with an empty SINR meter; QENG always carries it. Only queries the modem when
// something is actually missing, so the fast daemon path is untouched otherwise.
func fillMissingSINR(info map[string]any) {
	blank := func(k string) bool {
		s, _ := info[k].(string)
		return s == "" || s == "0" || s == "0.0"
	}
	present := func(k string) bool {
		s, _ := info[k].(string)
		return s != ""
	}
	needLTE := present("rsrp") && blank("snr")
	needNR := present("rsrp_5g") && blank("snr_5g")
	if !needLTE && !needNR {
		return
	}
	raw, err := atQuery([]string{`AT+QENG="servingcell"`}, 500*time.Millisecond)
	if err != nil {
		return
	}
	lte, nr := parseQENGSINR(raw)
	if needLTE && lte != "" {
		info["snr"] = lte
	}
	if needNR && nr != "" {
		info["snr_5g"] = normalizeNrSinr(nr)
	}
}

// parseQENGSINR pulls the LTE and NR SINR out of an AT+QENG="servingcell" reply.
// Per the Quectel spec the LTE serving line ends ...,<rsrp>,<rsrq>,<rssi>,<sinr>,
// so SINR is field 13 after the "LTE"," tag (matching getCellularInfoViaAT's own
// LTE parse). NR SINR is field 4 on the NR5G-NSA/standalone-SA line, or field 13
// on the combined "servingcell",...,"NR5G-SA",... form.
func parseQENGSINR(raw string) (lte, nr string) {
	if f := lteQENGFields(raw); len(f) >= 14 {
		lte = f[13]
	}
	if f := qengFields(raw, `"NR5G-NSA",`); len(f) >= 8 {
		nr = f[4]
	} else if f := qengFields(raw, `"NR5G-SA",`); len(f) >= 8 {
		nr = f[4]
	} else if f := qengFields(raw, `"servingcell",`); len(f) >= 14 && f[1] == "NR5G-SA" {
		nr = f[13]
	}
	return lte, nr
}

var (
	pingRecvRe = regexp.MustCompile(`(\d+) packets received`)
	pingLossRe = regexp.MustCompile(`(\d+)% packet loss`)
	pingAvgRe  = regexp.MustCompile(`=\s*[\d.]+/([\d.]+)/`)
)

// checkInternet probes real upstream connectivity from the router itself, not
// from the panel host: the modem can be registered with an IP yet carry no
// working data (dead APN, upstream outage), which the stock "Registered" state
// never shows. It pings 8.8.8.8 (raw IP path) and resolves a hostname (DNS), so
// the panel can say whether the internet is actually reachable right now.
func checkInternet() (map[string]any, error) {
	raw, err := run(`ping -c 2 -W 2 8.8.8.8 2>&1; echo "---DNS---"; nslookup one.one.one.one 2>&1`, 15*time.Second)
	if err != nil {
		return nil, err
	}
	pingOut, dnsOut := raw, ""
	if i := strings.Index(raw, "---DNS---"); i >= 0 {
		pingOut, dnsOut = raw[:i], raw[i:]
	}
	res := map[string]any{"online": false, "dns": false}
	if m := pingRecvRe.FindStringSubmatch(pingOut); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 {
			res["online"] = true
		}
	}
	if m := pingLossRe.FindStringSubmatch(pingOut); m != nil {
		res["loss"], _ = strconv.Atoi(m[1])
	}
	if m := pingAvgRe.FindStringSubmatch(pingOut); m != nil {
		if v, e := strconv.ParseFloat(m[1], 64); e == nil {
			res["latency_ms"] = v
		}
	}
	// one.one.one.one resolves to 1.1.1.1 — its presence means DNS works.
	if strings.Contains(dnsOut, "1.1.1.1") {
		res["dns"] = true
	}
	return res, nil
}

// loose is a JSON scalar a firmware may report as a quoted string ("-77.0"),
// a bare number (-77.0), a placeholder ("-", "") when it has no value, or
// null. It stores whatever arrives as a trimmed string and never errors, so
// one field a given firmware formats differently can't sink the whole parse
// — a plain string field rejects numbers, and json.Number rejects "-".
type loose string

func (l *loose) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*l = ""
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	*l = loose(strings.TrimSpace(s))
	return nil
}

func (l loose) String() string { return string(l) }

// simStatusName maps the daemon's numeric SIM status to a label.
func simStatusName(code int) string {
	switch code {
	case 0:
		return "Absent"
	case 1:
		return "Ready"
	case 2:
		return "PIN required"
	case 3:
		return "PUK required"
	case 4:
		return "Error"
	default:
		return fmt.Sprintf("code %d", code)
	}
}

// parseDumpStatus turns the stock daemon's dump_status JSON into the panel's
// cellular map. Signal fields use loose so a firmware that reports them as
// numbers (or "-") parses the same as one that reports quoted strings.
func parseDumpStatus(raw string) (map[string]any, error) {
	var parsed struct {
		SIM struct {
			Status     int    `json:"status"`
			PinRemains int    `json:"pin_remains"`
			PukRemains int    `json:"puk_remains"`
			Lock       int    `json:"lock"`
			ICCID      string `json:"iccid"`
			IMSI       string `json:"imsi"`
			Number     string `json:"number"`
			Country    string `json:"country"`
		} `json:"sim"`
		Status struct {
			Registration int    `json:"registration"`
			ISP          string `json:"isp"`
			APN          string `json:"apn"`
			RAT          string `json:"rat"`
			CellBand     string `json:"cell_band"`
			Cell5GBand   string `json:"cell_band_5g"`
			Level        int    `json:"level"`
			RSRP         loose  `json:"rsrp"`
			RSRQ         loose  `json:"rsrq"`
			SNR          loose  `json:"snr"`
			RSSI         loose  `json:"rssi"`
			RSRP5G       loose  `json:"rsrp_5g"`
			RSRQ5G       loose  `json:"rsrq_5g"`
			SNR5G        loose  `json:"snr_5g"`
			PCI          loose  `json:"pci"`
			PCI5G        loose  `json:"pci_5g"`
			Roam         int    `json:"roam"`
		} `json:"status"`
		Network struct {
			V4Addr string `json:"v4addr"`
			V4GW   string `json:"v4gw"`
			V4Mask string `json:"v4mask"`
			V4DNS1 string `json:"v4dns1"`
			V4DNS2 string `json:"v4dns2"`
			V4MTU  int    `json:"v4mtu"`
			V6Addr string `json:"v6addr"`
			V6GW   string `json:"v6gw"`
			V6DNS1 string `json:"v6dns1"`
			V6DNS2 string `json:"v6dns2"`
			V6MTU  int    `json:"v6mtu"`
		} `json:"network"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, routerErrf("Could not parse mobile status: %v", err)
	}
	s := parsed.Status
	// Aggregated CA bands (e.g. "B3+B8+n1+n78") if the modem replies, otherwise
	// stick with the human-readable primary band from the daemon. The same
	// QCAINFO call also yields the primary carrier's ARFCN/EARFCN, which the
	// daemon's dump_status doesn't expose.
	band := s.CellBand
	earfcn, nrArfcn := "", ""
	if agg, arf, rat := aggregatedBands(); agg != "" || arf != "" {
		if agg != "" {
			band = agg
		}
		switch rat {
		case "LTE":
			earfcn = arf
		case "NR5G":
			nrArfcn = arf
		}
	}
	return map[string]any{
		"operator":     s.ISP,
		"network_type": s.RAT,
		"band":         band,
		"band_primary": s.CellBand,
		"band_5g":      s.Cell5GBand,
		"earfcn":       earfcn,
		"nr_arfcn":     nrArfcn,
		"apn":          s.APN,
		"level":        s.Level,
		"rsrp":         s.RSRP.String(),
		"rsrq":         s.RSRQ.String(),
		"snr":          s.SNR.String(),
		"rssi":         s.RSSI.String(),
		"rsrp_5g":      s.RSRP5G.String(),
		"rsrq_5g":      s.RSRQ5G.String(),
		"snr_5g":       normalizeNrSinr(s.SNR5G.String()),
		"pci":          s.PCI.String(),
		"pci_5g":       s.PCI5G.String(),
		"roaming":      s.Roam != 0,
		"registered":   s.Registration == 1,
		"sim_status":   simStatusName(parsed.SIM.Status),
		"sim_locked":   parsed.SIM.Lock == 1,
		"sim_pin_left": parsed.SIM.PinRemains,
		"sim_puk_left": parsed.SIM.PukRemains,
		"sim_number":   simNumber(parsed.SIM.Number),
		"sim_iccid":    parsed.SIM.ICCID,
		"sim_country":  parsed.SIM.Country,
		"wan_v4":       parsed.Network.V4Addr,
		"wan_v4_gw":    parsed.Network.V4GW,
		"wan_v4_mask":  parsed.Network.V4Mask,
		"wan_v4_dns":   joinNonEmpty(", ", parsed.Network.V4DNS1, parsed.Network.V4DNS2),
		"wan_v4_mtu":   parsed.Network.V4MTU,
		"wan_v6":       cleanV6(parsed.Network.V6Addr),
		"wan_v6_gw":    cleanV6(parsed.Network.V6GW),
		"wan_v6_dns":   joinNonEmpty(", ", cleanV6(parsed.Network.V6DNS1), cleanV6(parsed.Network.V6DNS2)),
		"wan_v6_mtu":   parsed.Network.V6MTU,
	}, nil
}

// cleanV6 drops the "all zeros" address the daemon prints when no IPv6 is up.
func cleanV6(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "0:0:0:0:0:0:0:0" || s == "::" {
		return ""
	}
	return s
}

// joinNonEmpty joins parts with sep, skipping empty ones.
func joinNonEmpty(sep string, parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// AT parsers for the cellular fallback. Formats confirmed live against the
// RG520N-EB modem; parsing is index/regex based and tolerant of missing lines.
var (
	copsRe    = regexp.MustCompile(`\+COPS:\s*\d+,\d+,"([^"]*)"`)
	cpinRe    = regexp.MustCompile(`\+CPIN:\s*([A-Z ]+)`)
	ceregRe   = regexp.MustCompile(`\+CEREG:\s*\d+,(\d+)`)
	c5gregRe  = regexp.MustCompile(`\+C5GREG:\s*\d+,(\d+)`)
	qccidRe   = regexp.MustCompile(`\+QCCID:\s*(\w+)`)
	cnumRe    = regexp.MustCompile(`\+CNUM:\s*[^,]*,"([^"]*)"`)
	cgcontRe  = regexp.MustCompile(`\+CGDCONT:\s*1,"[^"]*","([^"]*)"`)
	qnwinfoRe = regexp.MustCompile(`\+QNWINFO:\s*"([^"]*)","[^"]*","([^"]*)"`)
	bandNumRe = regexp.MustCompile(`BAND\s+(\d+)`)
)

// bandLabel turns a QNWINFO band name ("LTE BAND 3") into a short label
// ("B3" for LTE, "n1" for NR5G). A band number of 0 is returned as "" (no band
// 0 exists): cb0401 v1 firmware reports "NR5G BAND 0" on a live cell, and
// leaving it blank lets the caller derive the real band from the ARFCN instead
// of surfacing a bogus "n0".
func bandLabel(s, prefix string) string {
	if m := bandNumRe.FindStringSubmatch(s); m != nil && m[1] != "0" {
		return prefix + m[1]
	}
	return ""
}

// qengFields returns the comma-separated fields that follow tag on a +QENG
// servingcell line (with surrounding quotes stripped), or nil if the line
// isn't present.
// lteQENGFields returns the LTE serving-cell fields from AT+QENG="servingcell"
// indexed like the separate EN-DC line (+QENG: "LTE","FDD",<MCC>,...). In
// LTE-only mode the modem folds them into the servingcell line instead
// (+QENG: "servingcell","NOCONN","LTE","FDD",<MCC>,...), two fields later.
func lteQENGFields(raw string) []string {
	if f := qengFields(raw, `"LTE","`); f != nil {
		return f
	}
	if f := qengFields(raw, `"servingcell",`); len(f) > 2 && f[1] == "LTE" {
		return f[2:]
	}
	return nil
}

func qengFields(raw, tag string) []string {
	head := "+QENG: " + tag
	i := strings.Index(raw, head)
	if i < 0 {
		return nil
	}
	rest := raw[i+len(head):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	parts := strings.Split(rest, ",")
	for j := range parts {
		parts[j] = strings.Trim(strings.TrimSpace(parts[j]), `"`)
	}
	return parts
}

// normalizeNrSinr fixes the NR SS-SINR unit on firmware that reports it in
// 0.1 dB steps (cb0401 v1 shows e.g. 195 for 19.5 dB) while others report whole
// dB. A real SINR never exceeds ~40 dB, so a magnitude above that is deci-dB and
// gets scaled down; normal values (and LTE SINR, which isn't passed here) are
// left untouched.
func normalizeNrSinr(s string) string {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return s
	}
	if v > 40 || v < -40 {
		return strconv.FormatFloat(v/10, 'f', -1, 64)
	}
	return s
}

// barsFromRSRP approximates the daemon's 0–5 signal level from an LTE RSRP.
func barsFromRSRP(s string) int {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	switch {
	case v >= -80:
		return 5
	case v >= -90:
		return 4
	case v >= -100:
		return 3
	case v >= -110:
		return 2
	default:
		return 1
	}
}

// getCellularInfoViaAT reconstructs the cellular card from the modem directly,
// for firmware whose stock daemon has no dump_status. It is best-effort: each
// field is filled only when its AT command answered, and anything missing is
// left blank rather than failing the call. This is how the base info was read
// before it moved onto the daemon, kept alive as a fallback for cb0401 v1.
func getCellularInfoViaAT() (map[string]any, error) {
	raw, err := atQuery([]string{
		"AT+COPS?",
		"AT+QNWINFO",
		`AT+QENG="servingcell"`,
		"AT+CPIN?",
		"AT+CEREG?",
		"AT+C5GREG?",
		"AT+QCCID",
		"AT+CNUM",
		"AT+CGDCONT?",
		`AT+QNWCFG="nr5g_meas_info"`, // read only: real NR PCI when QENG says 0
	}, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(raw, "+") {
		return nil, routerErrf("modem returned no usable AT data")
	}
	return parseATCellular(raw), nil
}

// parseATCellular builds the cellular map from a raw AT reply block. Split out
// from getCellularInfoViaAT so it can be tested against captured modem output.
func parseATCellular(raw string) map[string]any {
	info := map[string]any{
		"operator": "", "network_type": "", "band": "", "band_primary": "",
		"band_5g": "", "apn": "", "level": 0,
		"earfcn": "", "nr_arfcn": "",
		"rsrp": "", "rsrq": "", "snr": "", "rssi": "",
		"rsrp_5g": "", "rsrq_5g": "", "snr_5g": "", "pci": "", "pci_5g": "",
		"roaming": false, "registered": false, "sim_status": "",
		"sim_locked": false, "sim_pin_left": 0, "sim_puk_left": 0,
		"sim_number": "", "sim_iccid": "", "sim_country": "",
	}

	if m := copsRe.FindStringSubmatch(raw); m != nil {
		info["operator"] = m[1]
	}
	if m := cpinRe.FindStringSubmatch(raw); m != nil {
		switch st := strings.TrimSpace(m[1]); {
		case st == "READY":
			info["sim_status"] = "Ready"
		case strings.Contains(st, "PUK"):
			info["sim_status"] = "PUK required"
			info["sim_locked"] = true
		case strings.Contains(st, "PIN"):
			info["sim_status"] = "PIN required"
			info["sim_locked"] = true
		default:
			info["sim_status"] = st
		}
	}
	if m := ceregRe.FindStringSubmatch(raw); m != nil {
		switch m[1] {
		case "1":
			info["registered"] = true
		case "5":
			info["registered"] = true
			info["roaming"] = true
		}
	}
	// 5G SA registers via 5GS registration (C5GREG), not EPS (CEREG); without
	// this a pure-SA cb0401 v1 reads as "not registered" even when connected.
	if reg, _ := info["registered"].(bool); !reg {
		if m := c5gregRe.FindStringSubmatch(raw); m != nil {
			switch m[1] {
			case "1":
				info["registered"] = true
			case "5":
				info["registered"] = true
				info["roaming"] = true
			}
		}
	}
	if m := qccidRe.FindStringSubmatch(raw); m != nil {
		info["sim_iccid"] = m[1]
	}
	if m := cnumRe.FindStringSubmatch(raw); m != nil {
		info["sim_number"] = m[1]
	}
	if m := cgcontRe.FindStringSubmatch(raw); m != nil {
		info["apn"] = m[1]
	}

	// Network type + per-leg bands from QNWINFO ("FDD LTE" / "FDD NR5G").
	hasLTE, hasNR := false, false
	for _, m := range qnwinfoRe.FindAllStringSubmatch(raw, -1) {
		rat, bandName := m[1], m[2]
		switch {
		case strings.Contains(rat, "NR5G"):
			hasNR = true
			info["band_5g"] = bandLabel(bandName, "n")
		case strings.Contains(rat, "LTE"):
			hasLTE = true
			info["band_primary"] = bandLabel(bandName, "B")
		}
	}
	switch {
	case hasLTE && hasNR:
		info["network_type"] = "5G NSA"
	case hasNR:
		info["network_type"] = "5G SA"
	case hasLTE:
		info["network_type"] = "LTE"
	}

	// Signal + PCI from QENG servingcell (LTE anchor and, in NSA, the NR leg).
	if f := lteQENGFields(raw); len(f) >= 14 {
		info["pci"] = f[4]
		info["earfcn"] = f[5]
		info["rsrp"] = f[10]
		info["rsrq"] = f[11]
		info["rssi"] = f[12]
		info["snr"] = f[13]
		if info["band_primary"] == "" {
			info["band_primary"] = "B" + f[6]
		}
		info["level"] = barsFromRSRP(f[10])
	}
	if f := qengFields(raw, `"NR5G-NSA",`); len(f) >= 8 {
		info["pci_5g"] = f[2]
		info["rsrp_5g"] = f[3]
		info["snr_5g"] = normalizeNrSinr(f[4])
		info["rsrq_5g"] = f[5]
		info["nr_arfcn"] = f[6]
		if info["band_5g"] == "" {
			info["band_5g"] = nrBandLabel(f[7], f[6])
		}
	}
	// NR5G-SA (no LTE anchor). This firmware reports each RAT on its own +QENG
	// line — as the NSA capture shows ("servingcell","NOCONN" + a separate
	// "LTE"/"NR5G-NSA" line) — so try a standalone "NR5G-SA" line first, with
	// the same field order as the NR5G-NSA leg. Fall back to the combined
	// "servingcell",<state>,"NR5G-SA",<dup>,<mcc>,<mnc>,<cellID>,<PCI>,<TAC>,
	// <ARFCN>,<band>,<bw>,<RSRP>,<RSRQ>,<SINR>,... form from the Quectel spec.
	// Both are best-effort; confirm indices against a live SA unit.
	if f := qengFields(raw, `"NR5G-SA",`); len(f) >= 8 {
		info["pci_5g"] = f[2]
		info["rsrp_5g"] = f[3]
		info["snr_5g"] = normalizeNrSinr(f[4])
		info["rsrq_5g"] = f[5]
		info["nr_arfcn"] = f[6]
		if info["band_5g"] == "" {
			info["band_5g"] = nrBandLabel(f[7], f[6])
		}
		info["level"] = barsFromRSRP(f[3])
	} else if f := qengFields(raw, `"servingcell",`); len(f) >= 14 && f[1] == "NR5G-SA" {
		info["pci_5g"] = f[6]
		info["nr_arfcn"] = f[8]
		info["rsrp_5g"] = f[11]
		info["rsrq_5g"] = f[12]
		info["snr_5g"] = normalizeNrSinr(f[13])
		if info["band_5g"] == "" {
			info["band_5g"] = nrBandLabel(f[9], f[8])
		}
		info["level"] = barsFromRSRP(f[11])
	}
	// v1 firmware reports PCI 0 for the NR leg; take the real one from the NR
	// measurement report, the same source the Cells-around card uses (#4).
	if info["pci_5g"] == "0" {
		if a, ok := atoiDash(info["nr_arfcn"].(string)); ok {
			if p := nrPCIFromMeas(raw, a); p > 0 {
				info["pci_5g"] = strconv.Itoa(p)
			}
		}
	}

	// Aggregated CA bands (same helper the daemon path uses), else compose
	// from the primary/5G legs. ARFCN/EARFCN here comes from QENG above, so the
	// QCAINFO ARFCN is ignored.
	if agg, _, _ := aggregatedBands(); agg != "" {
		info["band"] = agg
		// In NSA, some cb0401 v1 firmware lists only the LTE carriers in
		// QCAINFO, so the 5G leg (known from QENG above) is missing from the
		// Carriers row - append it when it isn't already there.
		if b5, _ := info["band_5g"].(string); b5 != "" {
			found := false
			for _, t := range strings.Split(agg, "+") {
				if t == b5 {
					found = true
					break
				}
			}
			if !found {
				info["band"] = agg + "+" + b5
			}
		}
	} else {
		bp, _ := info["band_primary"].(string)
		b5, _ := info["band_5g"].(string)
		switch {
		case bp != "" && b5 != "":
			info["band"] = bp + "+" + b5
		case bp != "":
			info["band"] = bp
		case b5 != "":
			info["band"] = b5
		}
	}
	return info
}

// setNr5gMode switches the panel's 5G mode:
//
//	0 = SA+NSA both enabled (modem picks NSA when available)
//	1 = SA disabled (NSA/LTE only)
//	2 = NSA disabled (force SA only, LTE fallback)
//	3 = all NR5G disabled (LTE only)
//
// 0-2 are AT+QNWPREFCFG="nr5g_disable_mode" with mode_pref left at LTE:NR5G.
// LTE only can't go through nr5g_disable_mode: the RG520N answers ERROR to
// value 3, so it's mode_pref=LTE instead - the same thing the stock UI's
// "4G only" writes. The stock network type (mobile.common.networktype) is
// kept in step, so the stock UI shows the same mode and the stock daemon
// doesn't put mode_pref back on its next restart; the mode hook re-asserts
// both on every ifup in case anything resets them.
func setNr5gMode(mode int) (map[string]string, error) {
	if mode < 0 || mode > 3 {
		return nil, fmt.Errorf("invalid 5G mode %d (0-3)", mode)
	}
	stockType := "auto"
	if mode == 3 {
		stockType = "4g"
	}
	// Best-effort: older stock firmware without this action still gets the
	// mode over AT below and from the hook.
	_ = setStockNetworkType(stockType)
	raw, err := atQuery(modeATCommands(mode), 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if strings.Contains(raw, "ERROR") || !strings.Contains(raw, "OK") {
		return nil, routerErrf("Modem rejected the 5G mode change: %s", strings.TrimSpace(raw))
	}
	if err := saveModePref(mode); err != nil {
		return nil, err
	}
	invalidate("stock:netcfg")
	return getModemConfig()
}

// modeATCommands are the AT writes for one panel 5G mode (see setNr5gMode).
func modeATCommands(mode int) []string {
	if mode == 3 {
		return []string{`AT+QNWPREFCFG="mode_pref",LTE`}
	}
	return []string{
		`AT+QNWPREFCFG="mode_pref",LTE:NR5G`,
		fmt.Sprintf(`AT+QNWPREFCFG="nr5g_disable_mode",%d`, mode),
	}
}

// setStockNetworkType sets the stock network type ("auto"/"4g") through the
// stock UI's own action, keeping the mobile-data and roaming switches as
// they are (the action takes all three).
func setStockNetworkType(t string) error {
	cur, err := stockCall(stockActions["netcfg"], map[string]string{}, 20*time.Second)
	if err != nil {
		return err
	}
	if s, _ := cur["networktype"].(string); s == t {
		return nil
	}
	_, err = stockCall(stockActions["netcfg_set"], map[string]string{
		"networktype": t,
		"networkdata": anyToStr(cur["networkdata"]),
		"networkroam": anyToStr(cur["networkroam"]),
	}, 45*time.Second)
	return err
}

// panelMode maps what the modem reports back to the panel's 5G mode:
// mode_pref=LTE is LTE only whatever nr5g_disable_mode says (that's how the
// stock UI's "4G only" looks), and mode_pref=NR5G is SA only.
func panelMode(modePref, disable string) string {
	switch strings.ToUpper(strings.TrimSpace(modePref)) {
	case "LTE":
		return "3"
	case "NR5G":
		return "2"
	}
	return disable
}

// normalizeBandList turns "1:3:7" / "1,3,7" (in any order, with dupes) into
// the daemon's "1,3,7" form. Only digits and separators get through, since
// the result ends up in a command run on the router.
func normalizeBandList(s string) (string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ',' || r == ' ' })
	seen := map[int]bool{}
	var nums []int
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 300 {
			return "", routerErrf("Invalid band %q", f)
		}
		if !seen[n] {
			seen[n] = true
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return "", nil
	}
	sort.Ints(nums)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ","), nil
}

// applyBandsViaDaemon hands the band lists to the stock mobile daemon - the
// same call the stock web UI's setCellularBand makes. The daemon programs
// the modem over QMI and saves the lists to UCI, and re-applies them on its
// own every time it starts, so nothing on our side has to re-assert them.
// This is the preferred path: it keeps the lists persistent across reconnect
// and reboot for free, and doesn't fight the daemon for the AT port.
//
// The daemon can return code -1 transiently while the modem is mid-reconnect
// (right after a mode change, an APN switch, or on first boot before it's
// registered), so we retry a few times. But on some firmware (cb0401 v1, ROM
// 3.0.116) the daemon rejects this call outright and -1 never clears - a
// persistent -1 is not distinguishable from a transient one here, so after a
// few tries we give up and let setBands fall back to the AT path.
func applyBandsViaDaemon(lte, sa, nsa string) error {
	payload, _ := json.Marshal(map[string]string{"method": "set", "lte_band": lte, "sa_band": sa, "nsa_band": nsa})
	b64 := base64.StdEncoding.EncodeToString(payload)
	const tries = 3
	var last string
	for attempt := 1; attempt <= tries; attempt++ {
		out, err := run(fmt.Sprintf(`ubus call mobile device "$(echo %s | base64 -d)"`, b64), 90*time.Second)
		if err != nil {
			return err
		}
		last = strings.TrimSpace(out)
		var resp struct {
			Code *int `json:"code"`
		}
		if json.Unmarshal([]byte(out), &resp) == nil && resp.Code != nil && *resp.Code == 0 {
			return nil
		}
		if attempt < tries {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
	}
	return routerErrf("the modem daemon rejected the band change after %d tries: %s", tries, last)
}

// applyBandsViaAT programs the band lists straight into the modem with
// AT+QNWPREFCFG, for firmware whose stock daemon rejects the ubus band call
// (cb0401 v1 returns {"code":-1}). It also writes the lists into the daemon's
// UCI, so if that daemon later re-reads UCI on a reconnect or reboot it
// re-applies the same bands instead of overwriting them from a stale config.
// Band lists arrive comma-separated (already validated as plain digits by
// normalizeBandList); QNWPREFCFG wants them colon-separated.
func applyBandsViaAT(lte, sa, nsa string) error {
	raw, err := atQuery([]string{
		fmt.Sprintf(`AT+QNWPREFCFG="lte_band",%s`, commaToColon(lte)),
		fmt.Sprintf(`AT+QNWPREFCFG="nr5g_band",%s`, commaToColon(sa)),
		fmt.Sprintf(`AT+QNWPREFCFG="nsa_nr5g_band",%s`, commaToColon(nsa)),
	}, 800*time.Millisecond)
	if err != nil {
		return err
	}
	if strings.Contains(raw, "ERROR") || strings.Count(raw, "OK") < 3 {
		return routerErrf("the modem did not accept the band lists: %s", strings.TrimSpace(raw))
	}
	// Best-effort persistence into the daemon's own config (comma-separated).
	_, _ = run(fmt.Sprintf(
		`uci set mobile.device.lte_band='%s'; uci set mobile.device.sa_band='%s'; uci set mobile.device.nsa_band='%s'; uci commit mobile 2>/dev/null; true`,
		lte, sa, nsa), 10*time.Second)
	return nil
}

func setBands(nr5gBand, nsaNr5gBand, lteBand string) (map[string]string, error) {
	lte, err := normalizeBandList(lteBand)
	if err != nil {
		return nil, err
	}
	sa, err := normalizeBandList(nr5gBand)
	if err != nil {
		return nil, err
	}
	nsa, err := normalizeBandList(nsaNr5gBand)
	if err != nil {
		return nil, err
	}
	if lte == "" || sa == "" || nsa == "" {
		return nil, routerErrf("Each list needs at least one band (to turn 5G off, use the 5G mode selector instead)")
	}
	if err := applyBands(lte, sa, nsa); err != nil {
		return nil, err
	}
	return getModemConfig()
}

// ------------------------------------------------------------------- WiFi ---

type wifiDevice struct{ uci, iface string }

var wifiDevices = map[string]wifiDevice{
	"2.4": {"wifi0", "wl1"},
	"5":   {"wifi1", "wl0"},
}

func freqToChannel24(freq int) (int, bool) {
	if freq >= 2412 && freq <= 2472 {
		return (freq-2412)/5 + 1, true
	}
	return 0, false
}

type scannedNetwork struct {
	freq   int
	signal float64
	hasSig bool
	ssid   string
}

var (
	scanBssRe    = regexp.MustCompile(`^BSS `)
	scanFreqRe   = regexp.MustCompile(`^freq:\s*(\d+)`)
	scanSignalRe = regexp.MustCompile(`^signal:\s*(-?[\d.]+)`)
	scanSsidRe   = regexp.MustCompile(`^SSID:\s*(.*)`)
)

type channelInfo struct {
	Channel         *int             `json:"channel"`
	Count           int              `json:"count"`
	StrongestSignal *float64         `json:"strongest_signal"`
	Networks        []map[string]any `json:"networks"`
}

type wifiScanResult struct {
	Band           string         `json:"band"`
	TotalNetworks  int            `json:"total_networks"`
	Channels       []channelInfo  `json:"channels"`
	Recommendation map[string]any `json:"recommendation"`
}

func scanWifiChannels(band string) (*wifiScanResult, error) {
	dev, ok := wifiDevices[band]
	if !ok {
		return nil, routerErrf("Unknown Wi-Fi band: %s", band)
	}
	raw, err := run(fmt.Sprintf("iw dev %s scan 2>&1", dev.iface), 25*time.Second)
	if err != nil {
		return nil, err
	}

	var networks []scannedNetwork
	var cur scannedNetwork
	haveCur := false
	flush := func() {
		if haveCur && cur.freq != 0 {
			networks = append(networks, cur)
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if scanBssRe.MatchString(line) {
			flush()
			cur = scannedNetwork{}
			haveCur = true
		}
		if m := scanFreqRe.FindStringSubmatch(line); m != nil {
			cur.freq, _ = strconv.Atoi(m[1])
		}
		if m := scanSignalRe.FindStringSubmatch(line); m != nil {
			cur.signal, _ = strconv.ParseFloat(m[1], 64)
			cur.hasSig = true
		}
		if m := scanSsidRe.FindStringSubmatch(line); m != nil {
			cur.ssid = m[1]
			if cur.ssid == "" {
				cur.ssid = "(hidden)"
			}
		}
	}
	flush()

	byChannel := map[int][]scannedNetwork{}
	var unknownChannel []scannedNetwork
	for _, n := range networks {
		if band == "2.4" {
			if ch, ok := freqToChannel24(n.freq); ok {
				byChannel[ch] = append(byChannel[ch], n)
				continue
			}
			unknownChannel = append(unknownChannel, n)
		} else {
			byChannel[n.freq] = append(byChannel[n.freq], n)
		}
	}

	var keys []int
	for k := range byChannel {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var channels []channelInfo
	addChannel := func(chPtr *int, nets []scannedNetwork) {
		var strongest *float64
		for _, n := range nets {
			if n.hasSig {
				v := n.signal
				if strongest == nil || v > *strongest {
					strongest = &v
				}
			}
		}
		sortedNets := append([]scannedNetwork(nil), nets...)
		sort.Slice(sortedNets, func(i, j int) bool {
			si, sj := -999.0, -999.0
			if sortedNets[i].hasSig {
				si = sortedNets[i].signal
			}
			if sortedNets[j].hasSig {
				sj = sortedNets[j].signal
			}
			return si > sj
		})
		var netsOut []map[string]any
		for _, n := range sortedNets {
			var sig any
			if n.hasSig {
				sig = n.signal
			}
			netsOut = append(netsOut, map[string]any{"ssid": n.ssid, "signal": sig})
		}
		channels = append(channels, channelInfo{
			Channel: chPtr, Count: len(nets), StrongestSignal: strongest, Networks: netsOut,
		})
	}
	for _, ch := range keys {
		c := ch
		addChannel(&c, byChannel[ch])
	}
	if len(unknownChannel) > 0 {
		addChannel(nil, unknownChannel)
	}

	var recommendation map[string]any
	if band == "2.4" {
		recommendation = recommend24Channel(networks)
	}

	return &wifiScanResult{
		Band: band, TotalNetworks: len(networks), Channels: channels, Recommendation: recommendation,
	}, nil
}

// signalWeight: how much a given neighboring network actually interferes -
// strong and close networks matter more than weak and distant ones. Networks
// near the noise floor (below -82 dBm) barely count: they hardly interfere,
// and an access point hears many more of them on its own operating channel
// than on the ones it only visits while scanning, so weighting them like
// real neighbors made whichever channel you were on look busiest.
func signalWeight(hasSignal bool, signal float64) float64 {
	if !hasSignal {
		return 0.3
	}
	if signal >= -60 {
		return 1.0
	}
	if signal >= -75 {
		return 0.5
	}
	if signal >= -82 {
		return 0.2
	}
	return 0.05
}

// recommend24Channel scores each of the three genuinely non-overlapping
// channels (1/6/11) by total interference from ALL networks seen -
// including ones on neighboring channels that partially overlap in
// spectrum (not just an exact match), weighted by signal strength.
func recommend24Channel(networks []scannedNetwork) map[string]any {
	candidates := []int{1, 6, 11}
	scores := map[string]float64{}
	best := candidates[0]
	bestScore := -1.0
	for _, cand := range candidates {
		score := 0.0
		for _, n := range networks {
			ch, ok := freqToChannel24(n.freq)
			if !ok {
				continue
			}
			diff := ch - cand
			if diff < 0 {
				diff = -diff
			}
			var overlap float64
			if diff == 0 {
				overlap = 1.0
			} else {
				overlap = float64(5-diff) / 5
				if overlap < 0 {
					overlap = 0
				}
			}
			if overlap == 0 {
				continue
			}
			score += overlap * signalWeight(n.hasSig, n.signal)
		}
		rounded := roundTo(score, 2)
		scores[strconv.Itoa(cand)] = rounded
		if bestScore < 0 || rounded < bestScore {
			bestScore = rounded
			best = cand
		}
	}
	return map[string]any{"best_channel": best, "scores": scores}
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int(v*p+0.5)) / p
}

func rebootRouter() map[string]any {
	// We don't wait for a reply - the connection will drop along with the
	// reboot itself, which is expected, not an error.
	_, _ = run("reboot &", 5*time.Second)
	return map[string]any{"rebooting": true}
}

const (
	versionSpoofTarget = "/usr/share/xiaoqiang/xiaoqiang_version"
	versionSpoofFake   = "/tmp/fake_version"
)

// spoofFirmwareVersion makes the stock web updater's downgrade check see
// version "0.0.1", by bind-mounting a doctored copy of the version file
// over the real one. Xiaomi's updater refuses to flash anything with a
// version number lower than the current one - since 0.0.1 is lower than
// any real firmware, this lifts that block so you can flash an older
// firmware file through the router's own web UI (Settings -> Update ->
// Local update).
//
// This is memory-only (mount --bind of a /tmp file): nothing is written
// to flash, and it reverts on its own the next time the router reboots.
// The real firmware file on disk is never touched or removed.
func spoofFirmwareVersion() (string, error) {
	cmds := []string{
		fmt.Sprintf("umount %s 2>/dev/null; true", versionSpoofTarget),
		fmt.Sprintf("cp %s %s", versionSpoofTarget, versionSpoofFake),
		fmt.Sprintf(`sed -i "s/option ROM '.*/option ROM '0.0.1'/g" %s`, versionSpoofFake),
		fmt.Sprintf("mount --bind %s %s", versionSpoofFake, versionSpoofTarget),
		fmt.Sprintf(`grep "option ROM" %s`, versionSpoofTarget),
	}
	out, err := run(strings.Join(cmds, " ; "), 15*time.Second)
	if err != nil {
		return "", err
	}
	if !strings.Contains(out, "0.0.1") {
		return "", routerErrf("The spoof did not take (the router still reports the old version): %s", strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// getRouterModel reads the hardware model ("CB0401" or "CB0401V2") and
// firmware version from the stock version file. The downgrade spoof above
// only rewrites the ROM line, so a spoofed router reports "0.0.1" here -
// which is accurate for as long as the spoof is active.
func getRouterModel() (map[string]string, error) {
	out, err := run(fmt.Sprintf(`sed -n "s/.*option HARDWARE '\\(.*\\)'/HW=\\1/p; s/.*option ROM '\\(.*\\)'/ROM=\\1/p" %s`, versionSpoofTarget), 10*time.Second)
	if err != nil {
		return nil, err
	}
	v := parseConf(out)
	hw := strings.ToUpper(v["HW"])
	model := hw
	if strings.HasPrefix(hw, "CB0401") {
		rev := strings.TrimPrefix(hw, "CB0401")
		if rev == "" {
			rev = "V1"
		}
		model = "CB0401 " + rev
	}
	return map[string]string{"model": model, "firmware": v["ROM"]}, nil
}

// Persisting the 5G SA/NSA mode, and on cb0401 v1 the band choice too. Where
// the stock mobile daemon accepts band changes it keeps them in UCI and
// re-applies them itself (applyBandsViaDaemon), so only nr5g_disable_mode needs
// help. But v1's daemon rejects the band call: cpe-box writes bands straight to
// the modem over AT, and that firmware resets NR bands to default on every
// reboot - so the hook re-asserts both the mode and (when present in
// band_prefs.conf) the band lists. A small hotplug hook runs on the modem WAN
// ifup, and a boot-time re-assert covers firmware whose interface it can't
// match. /etc/hotplug.d lives on ramfs, so the patch script (run from a
// firewall include at every boot) re-creates the symlink to the persistent
// hook. The hook/patch layout is adapted from davidohne/xiaomi_cb0401
// (https://github.com/davidohne/xiaomi_cb0401/tree/main/Band_Unlock).
const (
	modePatchScript = `#!/bin/sh

[ -e "/tmp/5g_band_patch.log" ] && exit 0

HOOK_SRC="/data/custom/hooks/99-set-5g-bands"
HOOK_DEST="/etc/hotplug.d/iface/99-set-5g-bands"

[ -x "$HOOK_SRC" ] || chmod 755 "$HOOK_SRC"

mkdir -p /etc/hotplug.d/iface

if [ ! -e "$HOOK_DEST" ] || [ ! -f "$HOOK_DEST" ]; then
    ln -sf "$HOOK_SRC" "$HOOK_DEST"
fi

# Re-assert the saved 5G mode at boot too (detached), not only from the ifup
# hook: some firmware (cb0401 v1) names the modem interface differently, so the
# hook never fires there. Let the stock daemon bring the link up first, then
# apply (twice, in case it set its own mode in between).
APPLY="/data/custom/hooks/apply-5g-mode.sh"
if [ -x "$APPLY" ]; then
    if command -v setsid >/dev/null 2>&1; then
        setsid sh -c "sleep 30; $APPLY; sleep 25; $APPLY" >/dev/null 2>&1 </dev/null &
    else
        ( sleep 30; $APPLY; sleep 25; $APPLY ) >/dev/null 2>&1 </dev/null &
    fi
fi

echo "5g mode hook installed" > /tmp/5g_band_patch.log
`
	// Goes through microcom (and its port lock) like the stock at_cmd.sh,
	// so it can't interleave with the stock daemon's own AT traffic. The ifup
	// gate matches the modem WAN under the names seen across cb0401 firmware
	// (v2 uses wan_2; v1 differs), and the boot patch above covers the rest.
	modeHookScript = `#!/bin/sh
# cpe-box: router hook v7 (5G mode + bands + LEDs)
[ "$ACTION" = "ifup" ] || exit 0
case "$INTERFACE" in wan_2|wan|wwan|wwan0|modem_wan|5g_wan) ;; *) exit 0 ;; esac
LEDS=""
[ -f /data/custom/hooks/band_prefs.conf ] && . /data/custom/hooks/band_prefs.conf
[ -x /data/custom/hooks/apply-5g-mode.sh ] && /data/custom/hooks/apply-5g-mode.sh
if [ "$LEDS" = "0" ]; then
` + ledsOffScript + `
fi
`
	// Applies the saved 5G mode (mode_pref + nr5g_disable_mode) and, on firmware that drops
	// NR bands to default on every reboot (cb0401 v1), the saved band lists too,
	// waiting for the port and the stock daemon's port lock first. Shared by the
	// ifup hook and the boot-time re-assert so the choice survives a reboot even
	// where the hook can't fire. Goes through microcom like the stock at_cmd.sh.
	// Band lists are only present in band_prefs.conf when the stock daemon
	// rejected the band change (v1) and cpe-box fell back to AT; on firmware
	// where the daemon keeps bands, they're absent here and left to the daemon.
	modeApplyScript = `#!/bin/sh
[ -f /data/custom/hooks/band_prefs.conf ] && . /data/custom/hooks/band_prefs.conf
[ -n "$NR5G_MODE$LTE_BAND$NR5G_BAND$NSA_NR5G_BAND" ] || exit 0
i=0
while [ $i -lt 60 ]; do [ -e /dev/ttyUSB2 ] && break; sleep 2; i=$((i+1)); done
[ -e /dev/ttyUSB2 ] || exit 0
L=/var/lock/LCK..ttyUSB2
# Waits out the port lock (clearing a stale one), and retries when microcom
# still found the port taken or the modem didn't answer OK; a write that
# never lands is logged instead of being skipped silently.
at() {
  t=0
  while [ $t -lt 3 ]; do
    n=0
    while [ -e "$L" ] && [ $n -lt 60 ]; do
      o=$(tr -d ' \n' < "$L" 2>/dev/null)
      [ -n "$o" ] && ! kill -0 "$o" 2>/dev/null && rm -f "$L" && break
      usleep 250000; n=$((n+1))
    done
    printf 'AT+QNWPREFCFG="%s",%s\r' "$1" "$2" | busybox microcom /dev/ttyUSB2 >/tmp/cpebox_hook_at 2>&1 &
    p=$!
    usleep 1200000
    kill $p 2>/dev/null; wait $p 2>/dev/null
    grep -q OK /tmp/cpebox_hook_at && { rm -f /tmp/cpebox_hook_at; return 0; }
    t=$((t+1)); sleep 2
  done
  logger -t cpe-box "hook: AT+QNWPREFCFG=$1,$2 failed: $(tr '\r\n' '  ' < /tmp/cpebox_hook_at)"
  rm -f /tmp/cpebox_hook_at
}
[ -n "$LTE_BAND" ] && at lte_band "$LTE_BAND"
[ -n "$NSA_NR5G_BAND" ] && at nsa_nr5g_band "$NSA_NR5G_BAND"
[ -n "$NR5G_BAND" ] && at nr5g_band "$NR5G_BAND"
# LTE only is mode_pref=LTE: the modem rejects nr5g_disable_mode=3.
case "$NR5G_MODE" in
  3) at mode_pref LTE ;;
  0|1|2) at mode_pref LTE:NR5G; at nr5g_disable_mode "$NR5G_MODE" ;;
esac
`
	modeHookMarker      = "router hook v7"
	modeFirewallSnippet = `
config include 'auto_5g_band_patch'
	option type 'script'
	option path '/data/etc/crontabs/patches/5g_band_patch.sh'
	option enabled '1'
`
	modePatchPath = "/data/etc/crontabs/patches/5g_band_patch.sh"
	modeHookPath  = "/data/custom/hooks/99-set-5g-bands"
	modeApplyPath = "/data/custom/hooks/apply-5g-mode.sh"
	modePrefsPath = "/data/custom/hooks/band_prefs.conf"
)

var hookMu sync.Mutex

// installModeHook writes the current mode hook and its boot-time patch
// script, registers the firewall include once, and links the hook now.
func installModeHook() error {
	if _, err := run("mkdir -p /data/etc/crontabs/patches /data/custom/hooks /etc/hotplug.d/iface", 10*time.Second); err != nil {
		return err
	}
	for path, content := range map[string]string{modePatchPath: modePatchScript, modeHookPath: modeHookScript, modeApplyPath: modeApplyScript} {
		b64 := base64.StdEncoding.EncodeToString([]byte(content))
		if _, err := run(fmt.Sprintf("echo %s | base64 -d > %s && chmod 755 %s", b64, path, path), 10*time.Second); err != nil {
			return err
		}
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(modeFirewallSnippet))
	cmd := fmt.Sprintf("grep -q auto_5g_band_patch /etc/config/firewall 2>/dev/null || echo %s | base64 -d >> /etc/config/firewall; rm -f /tmp/5g_band_patch.log; sh %s", b64, modePatchPath)
	_, err := run(cmd, 15*time.Second)
	return err
}

// migrateLegacyBandHook upgrades routers set up by earlier versions:
//   - v1 hooks (0.2.x-0.3.2) re-wrote the 5G bands over AT on every reconnect
//     from band_prefs.conf. Those bands are handed to the stock
//     daemon instead (so they survive once the old hook stops writing them)
//     and band_prefs.conf is rewritten with just the mode.
//   - v2 hook (0.3.3 dev builds) handled the mode only; its prefs are kept as they are.
//
// Either way the script is then replaced by the current one. Safe to call
// repeatedly; does nothing on routers without a hook or with the current one.
func migrateLegacyBandHook() error {
	hookMu.Lock()
	defer hookMu.Unlock()
	return migrateLegacyBandHookLocked()
}

func migrateLegacyBandHookLocked() error {
	raw, err := run(fmt.Sprintf(`[ -f %[1]s ] || { echo NOHOOK; exit 0; }; grep -q %[2]q %[1]s && { echo CURRENT; exit 0; }
grep -q nsa_nr5g_band %[1]s && echo V1 || echo V2; cat %[3]s 2>/dev/null`, modeHookPath, modeHookMarker, modePrefsPath), 10*time.Second)
	if err != nil {
		return err
	}
	switch strings.SplitN(strings.TrimSpace(raw), "\n", 2)[0] {
	case "V2":
		return installModeHook()
	case "V1":
	default:
		return nil
	}
	prefs := parseConf(raw)
	u, err := readUciBands()
	if err != nil {
		return err
	}
	sa, _ := normalizeBandList(strings.Trim(prefs["NR5G_BAND"], `"'`))
	nsa, _ := normalizeBandList(strings.Trim(prefs["NSA_NR5G_BAND"], `"'`))
	curSA, _ := normalizeBandList(u["SA"])
	curNSA, _ := normalizeBandList(u["NSA"])
	lte, _ := normalizeBandList(u["LTE"])
	if sa == "" {
		sa = curSA
	}
	if nsa == "" {
		nsa = curNSA
	}
	if lte != "" && sa != "" && nsa != "" && (sa != curSA || nsa != curNSA) {
		if err := applyBandsViaDaemon(lte, sa, nsa); err != nil {
			return err
		}
	}
	// The earliest band hooks forced SA+NSA (mode 0); later ones read the
	// mode from band_prefs.conf too.
	mode := prefs["NR5G_MODE"]
	if len(mode) != 1 || mode < "0" || mode > "3" {
		mode = "0"
	}
	if _, err := run(fmt.Sprintf("printf 'NR5G_MODE=%s\\n' > %s", mode, modePrefsPath), 10*time.Second); err != nil {
		return err
	}
	return installModeHook()
}

var prefKeyRe = regexp.MustCompile(`^[A-Z0-9_]+$`)

// Hook pref values are either a single mode digit or a colon-separated band
// list (e.g. "1:3:7:28"); both are plain lowercase/digits plus ':'.
var prefValRe = regexp.MustCompile(`^[a-z0-9:]*$`)

// setHookPref records one setting for the router hook to re-apply,
// installing (or upgrading) the hook first. The whole read-modify-write
// runs under hookMu so two settings saved at once can't drop each other.
func setHookPref(key, value string) error {
	if !prefKeyRe.MatchString(key) || !prefValRe.MatchString(value) {
		return fmt.Errorf("invalid hook pref %s=%s", key, value)
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	if err := migrateLegacyBandHookLocked(); err != nil {
		return err
	}
	out, err := run(fmt.Sprintf("[ -f %s ] && echo 1 || echo 0", modeHookPath), 10*time.Second)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "1" {
		if err := installModeHook(); err != nil {
			return err
		}
	}
	_, err = run(fmt.Sprintf("touch %[1]s; sed -i '/^%[2]s=/d' %[1]s; echo '%[2]s=%[3]s' >> %[1]s", modePrefsPath, key, value), 10*time.Second)
	return err
}

func saveModePref(mode int) error { return setHookPref("NR5G_MODE", strconv.Itoa(mode)) }

// saveBandPrefs records the band lists (comma-separated in, stored colon-
// separated for AT) so the boot hook re-asserts them on firmware that resets
// NR bands to default on reboot. clearBandPrefs removes them again when the
// stock daemon took the change and will keep it itself.
func saveBandPrefs(lte, sa, nsa string) error {
	if err := setHookPref("LTE_BAND", commaToColon(lte)); err != nil {
		return err
	}
	if err := setHookPref("NR5G_BAND", commaToColon(sa)); err != nil {
		return err
	}
	return setHookPref("NSA_NR5G_BAND", commaToColon(nsa))
}

func clearBandPrefs() error {
	hookMu.Lock()
	defer hookMu.Unlock()
	_, err := run(fmt.Sprintf(
		"[ -f %[1]s ] && sed -i -e '/^LTE_BAND=/d' -e '/^NR5G_BAND=/d' -e '/^NSA_NR5G_BAND=/d' %[1]s || true",
		modePrefsPath), 10*time.Second)
	return err
}

// applyBands programs the three band lists: through the stock daemon when it
// accepts them, else straight over AT (cb0401 v1, whose daemon rejects the
// ubus band call). On the AT path it records the lists for the boot hook to
// re-assert (v1 forgets NR bands on reboot); on the daemon path it clears any
// such record, since the daemon persists bands itself.
func applyBands(lte, sa, nsa string) error {
	if err := applyBandsViaDaemon(lte, sa, nsa); err != nil {
		if atErr := applyBandsViaAT(lte, sa, nsa); atErr != nil {
			return routerErrf("%v; AT fallback also failed: %v", err, atErr)
		}
		return saveBandPrefs(lte, sa, nsa)
	}
	_ = clearBandPrefs()
	return nil
}

// Front LEDs, through the stock firmware's own switch (led_ctl, which the
// stock web UI calls): it persists the choice in UCI and the stock LED
// service honours it for the status and Wi-Fi LEDs. The 4G/5G signal LEDs
// aren't covered by it - the stock signal script re-lights them on every
// signal change - so those are "hung up" in the LED service too, and the
// router hook repeats that after a reboot. The stock "on" path calls an
// LED function this model doesn't define, which left the Wi-Fi LED dark,
// so turning back on also lifts every hang-up and re-triggers the current
// signal LED explicitly.
const (
	ledNetFuncs   = "net_4g_signal_good net_4g_signal_poor net_5g_signal_good net_5g_signal_poor"
	ledsOffScript = `led_ctl led_off >/dev/null 2>&1
for f in ` + ledNetFuncs + `; do xqled hangup $f; done
xqled net_4g_off; xqled net_5g_off`
	ledsOnScript = `led_ctl led_on >/dev/null 2>&1
for f in func_off wifi_off sys_off ` + ledNetFuncs + `; do xqled resume $f; done
xqled sys_ok; xqled wifi_on
S=$(ubus call mobile dump_status 2>/dev/null)
T=4G; echo "$S" | grep -q '"rat": "5G' && T=5G
LV=$(echo "$S" | sed -n 's/.*"level": \([0-9]*\).*/\1/p' | head -1)
SIG=WEAK; [ "${LV:-0}" -ge 3 ] && SIG=STRONG
ACTION=led TYPE=$T SIGNAL=$SIG sh /lib/mobile.d/01-signal_led.sh`
)

func getLeds() (map[string]any, error) {
	out, err := run(fmt.Sprintf(`echo "BLUE=$(uci -q get xiaoqiang.common.BLUE_LED)"; grep '^LEDS=' %s 2>/dev/null || true`, modePrefsPath), 10*time.Second)
	if err != nil {
		return nil, err
	}
	v := parseConf(out)
	return map[string]any{"on": v["BLUE"] != "0" && v["LEDS"] != "0"}, nil
}

func setLeds(on bool) (map[string]any, error) {
	val, script := "1", ledsOnScript
	if !on {
		val, script = "0", ledsOffScript
	}
	if err := setHookPref("LEDS", val); err != nil {
		return nil, err
	}
	if _, err := run(script, 30*time.Second); err != nil {
		return nil, err
	}
	return getLeds()
}

const bandsUnlockedMarker = "/data/custom/hooks/.bands_unlocked"

// provisionRouter is what setup.sh/setup.ps1 run once the GUI is built
// (`cpe-box --provision`): upgrade a legacy band hook, install
// the 5G mode hook, and on the very first run enable every band the modem
// hardware actually supports. That first-run unlock is recorded on the
// router, so re-running setup never overrides a band choice made since.
func provisionRouter() error {
	if err := migrateLegacyBandHook(); err != nil {
		return fmt.Errorf("migrating legacy band hook: %w", err)
	}
	if err := installModeHook(); err != nil {
		return fmt.Errorf("installing 5G mode hook: %w", err)
	}
	fmt.Println("5G mode hook installed (keeps the SA/NSA mode across reboots).")

	done, err := run(fmt.Sprintf("[ -f %s ] && echo 1 || echo 0", bandsUnlockedMarker), 10*time.Second)
	if err != nil {
		return err
	}
	if strings.TrimSpace(done) == "1" {
		fmt.Println("Bands were already unlocked by an earlier setup run - keeping your current band selection.")
		return nil
	}
	u, err := readUciBands()
	if err != nil {
		return err
	}
	var hw *struct{ lte, nr string }
	for prefix, sup := range modemBandSupport {
		if strings.HasPrefix(u["MODEL"], prefix) {
			s := sup
			hw = &s
		}
	}
	if hw == nil {
		fmt.Printf("Unknown modem model %q - not changing bands; pick them in the GUI.\n", u["MODEL"])
		return nil
	}
	fmt.Println("Enabling every band this modem supports (the modem may briefly reconnect)...")
	if err := applyBands(hw.lte, hw.nr, hw.nr); err != nil {
		// Provisioning bands is polish for the first run - the modem is
		// working already with whatever bands the stock daemon set at
		// factory. Failing here (usually because the modem is still
		// mid-reconnect from a firmware boot or a previous band change)
		// mustn't kill the whole setup: everything else - the SSH bridge,
		// the notification hooks, the panel itself - is done, and the
		// user can retry from Cellular > Bands whenever the modem is idle.
		// We deliberately don't touch the marker so the next setup run
		// tries again automatically.
		fmt.Printf("WARNING: couldn't unlock every band right now (%v).\n", err)
		fmt.Println("         The setup itself succeeded; pick bands from Cellular > Bands in the panel.")
		return nil
	}
	if _, err := run("touch "+bandsUnlockedMarker, 10*time.Second); err != nil {
		return err
	}
	fmt.Printf("Bands unlocked: LTE %s / 5G SA+NSA %s (kept across reboots).\n", hw.lte, hw.nr)
	return nil
}

var (
	wifiInfoChanRe = regexp.MustCompile(`channel (\d+) \((\d+) MHz\), width: (\d+) MHz`)
	wifiInfoSsidRe = regexp.MustCompile(`ssid (\S+)`)
	wifiInfoTxpRe  = regexp.MustCompile(`txpower ([\d.]+) dBm`)
)

func getWifiStatus() map[string]map[string]any {
	result := map[string]map[string]any{}
	for band, dev := range wifiDevices {
		info, _ := run(fmt.Sprintf("iw dev %s info 2>&1", dev.iface), 0)
		var ssid, clients any
		var channel, freqMhz, widthMhz, txpower any
		if m := wifiInfoChanRe.FindStringSubmatch(info); m != nil {
			c, _ := strconv.Atoi(m[1])
			f, _ := strconv.Atoi(m[2])
			w, _ := strconv.Atoi(m[3])
			channel, freqMhz, widthMhz = c, f, w
		}
		if m := wifiInfoSsidRe.FindStringSubmatch(info); m != nil {
			ssid = m[1]
		}
		if m := wifiInfoTxpRe.FindStringSubmatch(info); m != nil {
			txpower, _ = strconv.ParseFloat(m[1], 64)
		}
		// The Qualcomm driver doesn't report stations to nl80211 (iw station
		// dump is always empty), only through its own wlanconfig.
		clientsRaw, _ := run(fmt.Sprintf(`wlanconfig %s list sta 2>/dev/null | grep -ciE '^[0-9a-f]{2}(:[0-9a-f]{2}){5} ' || true`, dev.iface), 0)
		clientsRaw = strings.TrimSpace(clientsRaw)
		if clientsRaw == "" {
			clientsRaw = "0"
		}
		clients = clientsRaw
		result[band] = map[string]any{
			"ssid": ssid, "channel": channel, "freq_mhz": freqMhz, "width_mhz": widthMhz,
			"txpower_dbm": txpower, "clients": clients, "uci_device": dev.uci,
		}
	}
	return result
}

var (
	wifiChannelRe = regexp.MustCompile(`^(auto|[0-9]{1,3})$`)
	wifiBwRe      = regexp.MustCompile(`^[0-9]{2,3}$`)
)

func setWifi(band string, channel, bw string) (map[string]map[string]any, error) {
	dev, ok := wifiDevices[band]
	if !ok {
		return nil, routerErrf("Unknown Wi-Fi band: %s", band)
	}
	if channel != "" && !wifiChannelRe.MatchString(channel) {
		return nil, routerErrf("Invalid channel: %s", channel)
	}
	if bw != "" && !wifiBwRe.MatchString(bw) {
		return nil, routerErrf("Invalid channel width: %s", bw)
	}
	var cmds []string
	if channel != "" {
		cmds = append(cmds, fmt.Sprintf("uci set wireless.%s.channel='%s'", dev.uci, channel))
	}
	if bw != "" {
		cmds = append(cmds, fmt.Sprintf("uci set wireless.%s.bw='%s'", dev.uci, bw))
	}
	if len(cmds) == 0 {
		return getWifiStatus(), nil
	}
	cmds = append(cmds, "uci commit wireless")
	if _, err := run(strings.Join(cmds, " ; "), 0); err != nil {
		return nil, err
	}
	runBg(fmt.Sprintf("wifi reload %s", dev.uci), 150*time.Second)
	time.Sleep(3 * time.Second)
	return getWifiStatus(), nil
}

// ------------------------------------------------------------- System health ---

func parseProcStatCPULine(line string) (idle, total int64) {
	fields := strings.Fields(line)[1:]
	nums := make([]int64, len(fields))
	for i, f := range fields {
		nums[i], _ = strconv.ParseInt(f, 10, 64)
	}
	idle = nums[3]
	if len(nums) > 4 {
		idle += nums[4]
	}
	for _, n := range nums {
		total += n
	}
	return idle, total
}

// getSystemHealth reads everything in one SSH round-trip. CPU usage needs
// two /proc/stat samples a second apart, taken on the router itself so
// network latency can't skew the interval. Memory comes from /proc/meminfo
// (MemAvailable - what's really free once reclaimable cache is counted):
// `free` output differs between firmware builds and on some it had no
// "-/+ buffers/cache" line at all, which left RAM blank.
func getSystemHealth() (map[string]any, error) {
	raw, err := run(`head -1 /proc/stat; sleep 1; head -1 /proc/stat; echo @@; cat /proc/loadavg; echo @@; cat /proc/meminfo; echo @@; cat /sys/class/thermal/thermal_zone*/temp 2>/dev/null; echo @@; cat /proc/uptime`, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(raw, "@@")
	for len(parts) < 5 {
		parts = append(parts, "")
	}
	result := map[string]any{}

	var cpuLines []string
	for _, l := range strings.Split(strings.TrimSpace(parts[0]), "\n") {
		if strings.HasPrefix(l, "cpu ") {
			cpuLines = append(cpuLines, l)
		}
	}
	if len(cpuLines) >= 2 {
		idle1, total1 := parseProcStatCPULine(cpuLines[0])
		idle2, total2 := parseProcStatCPULine(cpuLines[1])
		if dt := total2 - total1; dt > 0 {
			result["cpu_pct"] = roundTo(100*(1-float64(idle2-idle1)/float64(dt)), 1)
		}
	}

	if f := strings.Fields(parts[1]); len(f) >= 3 {
		l1, _ := strconv.ParseFloat(f[0], 64)
		l5, _ := strconv.ParseFloat(f[1], 64)
		l15, _ := strconv.ParseFloat(f[2], 64)
		result["load1"], result["load5"], result["load15"] = l1, l5, l15
	}

	mem := map[string]int64{}
	for _, l := range strings.Split(parts[2], "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			mem[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	if total := mem["MemTotal"]; total > 0 {
		avail, ok := mem["MemAvailable"]
		if !ok {
			avail = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
		}
		result["mem_total_kb"] = total
		result["mem_free_real_kb"] = avail
		result["mem_used_kb"] = total - avail
	}

	var temps []int64
	for _, t := range strings.Fields(parts[3]) {
		if v, e := strconv.ParseInt(t, 10, 64); e == nil {
			temps = append(temps, v)
		}
	}
	if len(temps) > 0 {
		var sum, max int64
		max = temps[0]
		for _, t := range temps {
			sum += t
			if t > max {
				max = t
			}
		}
		// this platform reports whole degrees Celsius directly (not milli-C)
		result["temp_c_max"] = max
		result["temp_c_avg"] = roundTo(float64(sum)/float64(len(temps)), 1)
	}

	if f := strings.Fields(parts[4]); len(f) > 0 {
		if v, e := strconv.ParseFloat(f[0], 64); e == nil {
			result["uptime_sec"] = int64(v)
		}
	}
	return result, nil
}

// TX power is NOT controlled by this panel - verified via both available
// paths: `iw set txpower fixed` runs without error but has zero effect
// (not even momentarily); the vendor `cfg80211tool s_txpow` returns
// EINVAL, and g_txpow/get_maxpower/get_minpower simply aren't implemented
// by this driver (they return 0). getWifiStatus() shows the real value
// read-only; there's nothing here to change it with.

// ------------------------------------------------------------ Device watch ---

const devmonDir = "/etc/crontabs/patches"

var (
	knownMacsFile  = devmonDir + "/known_macs.txt"
	notifyConfFile = devmonDir + "/notify.conf"
	macRe          = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)
)

func parseConf(raw string) map[string]string {
	conf := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		conf[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return conf
}

type notifyConfig struct {
	Backend          string `json:"backend"`
	NtfyTopic        string `json:"ntfy_topic"`
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   string `json:"telegram_chat_id"`
	SmsForward       bool   `json:"sms_forward"`
}

// getNotifyConfig reads the router's live notify.conf - the single source
// of truth for which backend (ntfy/Telegram) is active, rather than
// trusting whatever environment variables this process happened to be
// launched with (those only matter as a fallback, for a router setup.sh
// hasn't touched yet).
func getNotifyConfig() notifyConfig {
	raw, _ := run("cat "+notifyConfFile+" 2>/dev/null || true", 0)
	conf := parseConf(raw)
	pick := func(key, envDefault string) string {
		if v, ok := conf[key]; ok && v != "" {
			return v
		}
		return envDefault
	}
	backend := pick("NOTIFY_BACKEND", notifyBackendEnv)
	if backend == "" {
		backend = "ntfy"
	}
	// Defaults to on: sms_notify.sh only exists to forward SMS, so an
	// installed-but-silently-doing-nothing daemon would be a more
	// surprising default than the reverse. The GUI checkbox is there for
	// anyone who'd rather it stayed off.
	smsForward := pick("SMS_FORWARD", "1") != "0"
	return notifyConfig{
		Backend:          backend,
		NtfyTopic:        pick("NTFY_TOPIC", ntfyTopicEnv),
		TelegramBotToken: pick("TELEGRAM_BOT_TOKEN", telegramTokenEnv),
		TelegramChatID:   pick("TELEGRAM_CHAT_ID", telegramChatEnv),
		SmsForward:       smsForward,
	}
}

// setNotifyConfig replaces the router's notify.conf outright -
// device_monitor.sh and command_watcher.sh read it fresh on every cron
// run, so nothing else needs restarting for a backend switch to take
// effect.
var (
	telegramTokenRe = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{20,}$`)
	telegramChatRe  = regexp.MustCompile(`^-?[0-9]{3,}$`)
	ntfyTopicRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
)

func setNotifyConfig(backend string, telegramBotToken, telegramChatID *string, smsForward *bool) (notifyConfig, error) {
	if backend != "ntfy" && backend != "telegram" {
		return notifyConfig{}, routerErrf(`Unknown notification backend: %q (expected "ntfy" or "telegram")`, backend)
	}
	current := getNotifyConfig()
	forward := current.SmsForward
	if smsForward != nil {
		forward = *smsForward
	}
	tok := current.TelegramBotToken
	if telegramBotToken != nil {
		tok = *telegramBotToken
	}
	chat := current.TelegramChatID
	if telegramChatID != nil {
		chat = *telegramChatID
	}
	tok, chat = strings.TrimSpace(tok), strings.TrimSpace(chat)
	if backend == "telegram" {
		// notify.conf is sourced by the router's shell scripts, so these
		// must never carry anything a shell would interpret.
		if !telegramTokenRe.MatchString(tok) {
			return notifyConfig{}, routerErrf("That doesn't look like a bot token (it's like 123456789:AAE...)")
		}
		if !telegramChatRe.MatchString(chat) {
			return notifyConfig{}, routerErrf("The chat ID is a number (like 123456789, or -100... for a group)")
		}
	}
	topic := current.NtfyTopic
	if backend == "ntfy" && !ntfyTopicRe.MatchString(topic) {
		// first switch to ntfy (e.g. set up with Telegram): make a private topic
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return notifyConfig{}, err
		}
		topic = "cpebox-" + hex.EncodeToString(b)
	}
	// A backend switch, or new/changed Telegram credentials, means whoever's
	// on the receiving end hasn't seen the command cheat-sheet before (or is
	// looking at a chat/topic that's never gotten one) - send it once, right
	// after the config that makes it deliverable actually lands.
	sendWelcome := backend != current.Backend ||
		(backend == "telegram" && (tok != current.TelegramBotToken || chat != current.TelegramChatID))

	forwardVal := "0"
	if forward {
		forwardVal = "1"
	}
	content := fmt.Sprintf("NOTIFY_BACKEND=%s\nNTFY_TOPIC=%s\nTELEGRAM_BOT_TOKEN=%s\nTELEGRAM_CHAT_ID=%s\nSMS_FORWARD=%s\n",
		backend, topic, tok, chat, forwardVal)
	if _, err := run("mkdir -p "+devmonDir, 0); err != nil {
		return notifyConfig{}, err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	if _, err := run(fmt.Sprintf("echo %s | base64 -d > %s && chmod 600 %s", b64, notifyConfFile, notifyConfFile), 0); err != nil {
		return notifyConfig{}, err
	}
	// setup.sh/setup.ps1 reuse these from .env on a re-run, so keep them in
	// step - otherwise re-running setup would undo a change made here.
	for k, v := range map[string]string{"NOTIFY_BACKEND": backend, "NTFY_TOPIC": topic, "TELEGRAM_BOT_TOKEN": tok, "TELEGRAM_CHAT_ID": chat} {
		_ = setEnvValue(k, v)
	}
	if sendWelcome {
		sendNotifyWelcomeMessage()
	}
	return getNotifyConfig(), nil
}

// sendNotifyWelcomeMessage sends a one-time cheat-sheet through whichever
// backend notify.conf now points at, right after it's set up or switched -
// so you see the available commands once, up front, instead of having to
// remember them or dig through the README the first time a device alert
// actually shows up. Best-effort: a failure here shouldn't fail the config
// save that triggered it (notify_common.sh may not be deployed yet if this
// is somehow called before the first full setup, for instance).
func sendNotifyWelcomeMessage() {
	msg := "Notifications are set up. When an unrecognized device joins your network, you'll get an alert here. Reply to it (or just message this chat) with:\n\n" +
		"trust - adds the last unrecognized device seen to the whitelist (no more alerts for it)\n" +
		"<MAC or IP> trust - whitelists a specific device\n" +
		"block - blocks the last unrecognized device seen\n" +
		"<MAC or IP> block - blocks a specific device\n" +
		"unblock / <MAC or IP> unblock - lifts a block without trusting the device\n" +
		"red alert - locks Wi-Fi to only whitelisted devices (briefly disconnects everyone, including trusted devices - there's no way around that on this hardware)\n" +
		"all clear (or red alert off) - undoes red alert, back to normal"
	b64 := base64.StdEncoding.EncodeToString([]byte(msg))
	cmd := fmt.Sprintf(`. %s/notify_common.sh && notify "Welcome" "wave" "$(echo %s | base64 -d)"`, devmonDir, b64)
	_, _ = run(cmd, 10*time.Second)
}

type deviceEntry struct {
	Mac      string  `json:"mac"`
	IP       *string `json:"ip"`
	Hostname *string `json:"hostname"`
	Vendor   *string `json:"vendor"`    // best-effort, from the device's MAC - see oui.go
	MdnsName *string `json:"mdns_name"` // best-effort, only looked up when Hostname is empty - see mdns.go
	Online   bool    `json:"online"`    // associated with Wi-Fi right now (or wired) - not just an unexpired DHCP lease
}

type deviceMonitorState struct {
	Devices   []deviceEntry `json:"devices"`
	Whitelist []string      `json:"whitelist"`
	Blocked   []string      `json:"blocked"` // MACs with a "Block <MAC>" firewall rule (panel lock / Telegram "block")
	Notify    notifyConfig  `json:"notify"`
}

func getDeviceMonitorState() (deviceMonitorState, error) {
	// The DHCP lease file lists every address the router has handed out that
	// hasn't expired yet - it says nothing about whether the device is
	// actually connected right now. For that we ask trafficd, which is the
	// router's own live view of Wi-Fi association and wired ARP: assoc=1
	// means the device is on the air; assoc=0 means the lease is still
	// valid but the device left. `ip neigh show` catches wired devices
	// that trafficd doesn't (a laptop on the LAN port); we only count
	// REACHABLE entries there - STALE means the kernel hasn't confirmed
	// the neighbour for a while (typical staleness is 30 s, hits several
	// minutes when the device left ungracefully), and DELAY is the
	// transient "check in progress" state that flips to STALE if the
	// answer never comes. Both would inflate the count with ghosts.
	raw, err := run(`cat /tmp/dhcp.leases 2>/dev/null || true
echo ---
ubus call trafficd hw '{"tree":false}' 2>/dev/null
echo ---
ip neigh show 2>/dev/null | awk '$5 ~ /^[0-9a-fA-F:]{17}$/ && $6 == "REACHABLE" {print $5}'`, 0)
	if err != nil {
		return deviceMonitorState{}, err
	}
	sections := strings.SplitN(raw, "\n---\n", 3)
	leasesRaw := sections[0]
	online := map[string]bool{}
	if len(sections) > 1 {
		// trafficd hw is a flat map of MAC -> {assoc: 0|1, ...}
		var hw map[string]struct {
			Assoc int `json:"assoc"`
		}
		if err := json.Unmarshal([]byte(sections[1]), &hw); err == nil {
			for mac, d := range hw {
				if d.Assoc == 1 {
					online[strings.ToUpper(mac)] = true
				}
			}
		}
	}
	if len(sections) > 2 {
		for _, mac := range strings.Fields(sections[2]) {
			online[strings.ToUpper(mac)] = true
		}
	}
	devices := []deviceEntry{}
	for _, line := range strings.Split(strings.TrimSpace(leasesRaw), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			mac := strings.ToUpper(parts[1])
			ip := parts[2]
			var hostname *string
			if parts[3] != "*" {
				h := parts[3]
				hostname = &h
			}
			entry := deviceEntry{Mac: mac, IP: &ip, Hostname: hostname, Online: online[mac]}
			if org, ok := macVendor(mac); ok {
				entry.Vendor = &org
			}
			devices = append(devices, entry)
		}
	}
	fillMdnsNames(devices)

	whitelistRaw, err := run("cat "+knownMacsFile+" 2>/dev/null || true", 0)
	if err != nil {
		return deviceMonitorState{}, err
	}
	seen := map[string]bool{}
	whitelist := []string{}
	for _, l := range strings.Split(strings.TrimSpace(whitelistRaw), "\n") {
		l = strings.ToUpper(strings.TrimSpace(l))
		if l != "" && !seen[l] {
			seen[l] = true
			whitelist = append(whitelist, l)
		}
	}
	sort.Strings(whitelist)

	// Devices blocked from the network: the firewall rules the Telegram
	// "block" command (router/command_watcher.sh) and the panel's lock both
	// write, named firewall.block_AA_BB_CC_DD_EE_FF with a src_mac.
	blockedRaw, _ := run(`uci -q show firewall 2>/dev/null | sed -n "s/^firewall\.block_[^.]*\.src_mac='\(.*\)'$/\1/p"`, 0)
	blocked := []string{}
	for _, l := range strings.Fields(blockedRaw) {
		if l = strings.ToUpper(l); macRe.MatchString(l) {
			blocked = append(blocked, l)
		}
	}
	sort.Strings(blocked)
	return deviceMonitorState{Devices: devices, Whitelist: whitelist, Blocked: blocked, Notify: getNotifyConfig()}, nil
}

// setDeviceBlocked blocks (or unblocks) a device from the network the same
// way the Telegram "block" command does: a firewall REJECT rule on its MAC
// from the LAN zone, kept across reboots in /etc/config/firewall.
func setDeviceBlocked(mac string, block bool) (deviceMonitorState, error) {
	mac = strings.ToUpper(strings.TrimSpace(mac))
	if !macRe.MatchString(mac) {
		return deviceMonitorState{}, routerErrf("Not a MAC address: %q", mac)
	}
	rule := "block_" + strings.ReplaceAll(mac, ":", "_")
	var cmd string
	if block {
		cmd = fmt.Sprintf(`uci -q delete firewall.%[1]s; uci set firewall.%[1]s=rule && uci set firewall.%[1]s.name='Block %[2]s' && uci set firewall.%[1]s.src='lan' && uci set firewall.%[1]s.src_mac='%[2]s' && uci set firewall.%[1]s.target='REJECT' && uci commit firewall && /etc/init.d/firewall reload >/dev/null 2>&1; echo OK`, rule, mac)
	} else {
		cmd = fmt.Sprintf(`uci -q delete firewall.%[1]s; uci commit firewall && /etc/init.d/firewall reload >/dev/null 2>&1; echo OK`, rule)
	}
	out, err := run(cmd, 30*time.Second)
	if err != nil {
		return deviceMonitorState{}, err
	}
	if !strings.Contains(out, "OK") {
		return deviceMonitorState{}, routerErrf("The router didn't confirm the change: %s", strings.TrimSpace(out))
	}
	return getDeviceMonitorState()
}

// setDeviceWhitelist fully replaces the MAC whitelist on the router
// (device_monitor.sh reads it fresh on every cron run - nothing else needs
// restarting).
func setDeviceWhitelist(macs []string) (deviceMonitorState, error) {
	seen := map[string]bool{}
	var clean []string
	for _, m := range macs {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" && macRe.MatchString(m) && !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Strings(clean)
	content := ""
	if len(clean) > 0 {
		content = strings.Join(clean, "\n") + "\n"
	}
	if _, err := run("mkdir -p "+devmonDir, 0); err != nil {
		return deviceMonitorState{}, err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	if _, err := run(fmt.Sprintf("echo %s | base64 -d > %s", b64, knownMacsFile), 0); err != nil {
		return deviceMonitorState{}, err
	}
	return getDeviceMonitorState()
}

// ------------------------------------------------------------ Root password ---

const saltAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomSalt(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	salt := make([]byte, n)
	for i, b := range raw {
		salt[i] = saltAlphabet[int(b)%len(saltAlphabet)]
	}
	return string(salt), nil
}

// setRootPassword changes the router's root password by computing a fresh
// MD5-crypt hash with the router's own `openssl` (so this doesn't need to
// reimplement crypt(3) locally) and writing it directly to
// /data/etc/shadow - NOT via the /etc/shadow symlink, which sits on this
// router's ramfs-mounted /etc and silently drops any change made through
// it on the next reboot. This is the fix that was found and confirmed to
// actually survive a reboot (unlike the naive `passwd`-style approach),
// after an earlier attempt without it quietly reverted to the factory
// password.
//
// The .env file is updated in the same call so the GUI's own self-heal
// fallback (which needs the current password to reinstall a lost SSH key)
// keeps working after this - a password change that only lives in the
// router and not here would make self-heal fail the next time it's needed.
func setRootPassword(newPassword string) error {
	if len(newPassword) < 4 {
		return routerErrf("Password should be at least 4 characters")
	}
	for _, c := range newPassword {
		if c < 0x20 || c == 0x7f {
			return routerErrf("The password can't contain line breaks or control characters")
		}
	}
	salt, err := randomSalt(8)
	if err != nil {
		return routerErrf("Could not generate a salt: %v", err)
	}
	pwB64 := base64.StdEncoding.EncodeToString([]byte(newPassword))
	// The password travels base64-encoded so arbitrary characters in it
	// (quotes, $, backticks, ...) can't interfere with the remote shell's
	// own quoting - openssl only ever sees it after the remote `base64 -d`
	// has decoded it back to the original bytes.
	cmd := fmt.Sprintf(
		`NEWHASH=$(openssl passwd -1 -salt %s "$(echo %s | base64 -d)") && sed -i "s#^root:[^:]*:#root:${NEWHASH}:#" /data/etc/shadow && echo PASSWORD_CHANGED`,
		salt, pwB64,
	)
	out, err := run(cmd, 10*time.Second)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "PASSWORD_CHANGED") {
		return routerErrf("The router did not confirm the change: %s", strings.TrimSpace(out))
	}
	routerPass = newPassword
	if err := setEnvValue("ROUTER_ROOT_PASSWORD", newPassword); err != nil {
		return routerErrf("Password changed on the router, but could not update .env (self-heal will use the old password until this is fixed): %v", err)
	}
	return nil
}

// ----------------------------------------------------------- Data usage ---

// getDataUsage reports today's and this-month's cellular usage - the
// mobile.flowstat.daily_usage / monthly_usage counters the modem keeps
// itself, which are the only totals on this SoC that catch traffic the
// hardware flow-offload path would otherwise hide from the kernel and
// from trafficd. Both are single totals (no rx/tx split from the modem),
// so to break them down we borrow the ratio from trafficd's WAN rx/tx
// byte totals - trafficd itself is HW-offload-blind so its absolute
// numbers can't be trusted, but the ratio between them is a fair proxy
// for how this line splits download vs upload over the router's lifetime.
// The live rx/tx rate can't come from trafficd either: its rx_rate stays at a
// few KB/s during a 30 MB/s download, in router and bridge mode alike. The
// modem's own netdevs (wwan0, and rmnet_mhi0 under it) do count every byte,
// so the rate is their /proc/net/dev delta over one second on the router.
func getDataUsage() (map[string]any, error) {
	raw, err := run(`ubus call trafficd wan
echo "DAY=$(uci -q get mobile.flowstat.daily_usage)"
echo "MONTH=$(uci -q get mobile.flowstat.monthly_usage)"
echo "EDAY=$(uci -q get mobile.flowstat.effective_day)"
N() { grep -E '^ *(`+modemNetdevs+`):' /proc/net/dev | sed "s/^/$1 /"; }
N NETDEV1; sleep 1; N NETDEV2`, 15*time.Second)
	if err != nil {
		return nil, err
	}
	shell := &strings.Builder{}
	jsonPart := &strings.Builder{}
	var netdev []string
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "DAY=") || strings.HasPrefix(line, "MONTH=") || strings.HasPrefix(line, "EDAY="):
			shell.WriteString(line)
			shell.WriteByte('\n')
		case strings.HasPrefix(line, "NETDEV"):
			netdev = append(netdev, line)
		default:
			jsonPart.WriteString(line)
			jsonPart.WriteByte('\n')
		}
	}
	v := parseConf(shell.String())
	num := func(k string) int64 { n, _ := strconv.ParseInt(v[k], 10, 64); return n }
	var wan struct {
		RxBytes int64 `json:"rx_bytes"`
		TxBytes int64 `json:"tx_bytes"`
		RxRate  int64 `json:"rx_rate"`
		TxRate  int64 `json:"tx_rate"`
	}
	_ = json.Unmarshal([]byte(jsonPart.String()), &wan)
	rxRatio := 0.8 // sensible default before trafficd has seen any bytes
	if sum := wan.RxBytes + wan.TxBytes; sum > 0 {
		rxRatio = float64(wan.RxBytes) / float64(sum)
	}
	out := map[string]any{}
	if v["DAY"] != "" {
		total := num("DAY")
		rx := int64(float64(total) * rxRatio)
		out["today"] = total
		out["today_rx"], out["today_tx"] = rx, total-rx
	}
	if v["MONTH"] != "" {
		total := num("MONTH")
		rx := int64(float64(total) * rxRatio)
		out["month"] = total
		out["month_rx"], out["month_tx"] = rx, total-rx
		out["month_start_day"] = num("EDAY")
	}
	if rx, tx, ok := netdevRate(netdev); ok {
		out["rx_rate"], out["tx_rate"] = rx, tx
	} else {
		out["rx_rate"], out["tx_rate"] = wan.RxRate, wan.TxRate
	}
	if v["DAY"] == "" && v["MONTH"] == "" {
		return nil, routerErrf("No data usage counters found on the router")
	}
	return out, nil
}

// --------------------------------------------------------------- Raw exec ---

// rawShell runs an arbitrary shell command - for the "advanced" tab. Use
// with care.
var (
	cscaRe  = regexp.MustCompile(`\+CSCA:\s*"([^"]*)"`)
	smscRe  = regexp.MustCompile(`^\+?[0-9]{3,20}$`)
	smscSep = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
)

// getSMSC reads the SMS service centre number the modem sends texts through
// (AT+CSCA, which the stock daemon's QMI path shares). Read over AT because
// the daemon's own get_smsc garbles the last digit on some numbers.
func getSMSC() (map[string]string, error) {
	raw, err := atQuery([]string{`AT+CSCA?`}, 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	m := cscaRe.FindStringSubmatch(raw)
	if m == nil {
		return nil, routerErrf("Modem didn't report an SMS centre: %s", strings.TrimSpace(raw))
	}
	return map[string]string{"smsc": m[1]}, nil
}

// setSMSC sets the SMS service centre number - for SIMs/providers whose
// default one doesn't deliver - and saves it (AT+CSAS) so it survives a
// reboot. International numbers (+...) go in as type 145, others as 129.
func setSMSC(num string) (map[string]string, error) {
	num = smscSep.Replace(strings.TrimSpace(num))
	if !smscRe.MatchString(num) {
		return nil, routerErrf("SMS centre must be a phone number, like +491710760000")
	}
	typ := 129
	if strings.HasPrefix(num, "+") {
		typ = 145
	}
	raw, err := atQuery([]string{fmt.Sprintf(`AT+CSCA="%s",%d`, num, typ), `AT+CSAS`}, 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if strings.Contains(raw, "ERROR") || !strings.Contains(raw, "OK") {
		return nil, routerErrf("Modem rejected the SMS centre: %s", strings.TrimSpace(raw))
	}
	return getSMSC()
}

// modemNetdevs are the modem's network devices across cb0401 firmware: wwan0
// is the WAN netdev on v2, rmnet_mhi0 the raw link under it (32-bit counters),
// the rmnet_data/rmnet names what other Quectel setups use.
const modemNetdevs = "wwan0|rmnet_mhi0|rmnet_data0|rmnet0"

// netdevRate turns two /proc/net/dev samples taken one second apart
// ("NETDEV1 wwan0: <rx bytes> ... <tx bytes> ...", then NETDEV2) into bytes/s.
// The busiest device wins: depending on the firmware and on bridge mode the
// traffic shows on wwan0 or only on the raw link. Counters that went
// backwards wrapped at 32 bits.
func netdevRate(lines []string) (rx, tx int64, ok bool) {
	type ctr struct{ rx, tx int64 }
	samples := map[string]map[string]ctr{"NETDEV1": {}, "NETDEV2": {}}
	for _, l := range lines {
		tag, rest, _ := strings.Cut(l, " ")
		name, fields, found := strings.Cut(rest, ":")
		f := strings.Fields(fields)
		if !found || len(f) < 9 || samples[tag] == nil {
			continue
		}
		r, err1 := strconv.ParseInt(f[0], 10, 64)
		t, err2 := strconv.ParseInt(f[8], 10, 64)
		if err1 == nil && err2 == nil {
			samples[tag][strings.TrimSpace(name)] = ctr{r, t}
		}
	}
	delta := func(a, b int64) int64 {
		if b < a {
			b += 1 << 32
		}
		if d := b - a; d >= 0 && d < 1<<32 {
			return d
		}
		return 0
	}
	for dev, a := range samples["NETDEV1"] {
		b, have := samples["NETDEV2"][dev]
		if !have {
			continue
		}
		dr, dt := delta(a.rx, b.rx), delta(a.tx, b.tx)
		if !ok || dr+dt > rx+tx {
			rx, tx, ok = dr, dt, true
		}
	}
	return rx, tx, ok
}

func rawShell(cmd string) (string, error) {
	return run(cmd, 30*time.Second)
}

// -------------------------------------------------------------- AT modem ---

var atCmdRe = regexp.MustCompile(`^AT[A-Za-z0-9+\-*_=,.?"@#$%&()!/ :;]{0,255}$`)

// sendAT sends one AT command to the Quectel modem over /dev/ttyUSB2 and
// returns everything the modem prints back (the echoed command, the
// unsolicited response lines, the final "OK"/"ERROR"), same shape the
// stock at_cmd.sh gets. Follows exactly the microcom pattern the stock
// firmware itself uses, so the modem-daemon side of things is untouched.
func sendAT(cmd string, wait time.Duration) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", routerErrf("Empty AT command")
	}
	if !atCmdRe.MatchString(cmd) {
		return "", routerErrf("AT commands must start with AT and contain only ASCII")
	}
	// Guard the shell side: pass the command via env, not interpolation.
	// microcom -t is milliseconds; give the modem a bit longer than wait so
	// we don't clip a slow reply, then kill in case it drops nothing at all.
	// Like atQuery it holds atMutex and waits out the port lock (clearing a
	// stale one): otherwise a cellular poll holding the port made microcom
	// fail with "can't create" on stderr and the console got an empty reply.
	ms := int(wait / time.Millisecond)
	if ms < 500 {
		ms = 500
	}
	script := fmt.Sprintf(`AT="$1"; O=/tmp/cpe_at_out.$$; L=/var/lock/LCK..ttyUSB2
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  [ -e $L ] || break
  OWNER=$(tr -d ' \n' < $L 2>/dev/null)
  [ -n "$OWNER" ] && ! kill -0 "$OWNER" 2>/dev/null && rm -f $L && break
  usleep 250000
done
( printf '%%s\r' "$AT" | busybox microcom -t %d %s > $O 2>&1 ) &
p=$!; sleep %d; kill $p 2>/dev/null; wait 2>/dev/null
cat $O
rm -f $O`, ms+300, atPort, (ms/1000)+1)
	// run() shells through ssh; pass the command as a positional arg via sh -s.
	quoted := strings.ReplaceAll(cmd, `'`, `'\''`)
	full := fmt.Sprintf(`sh -c '%s' _ '%s'`, strings.ReplaceAll(script, `'`, `'\''`), quoted)
	atMutex.Lock()
	defer atMutex.Unlock()
	var out string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		out, err = run(full, wait+10*time.Second)
		if err != nil || !strings.Contains(out, "can't create") {
			return out, err
		}
		time.Sleep(time.Second)
	}
	return "", routerErrf("Modem AT port stayed busy, try again")
}
