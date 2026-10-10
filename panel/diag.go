package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------- diagnostics ---
// Ping / traceroute and a speed test, all run on the router itself, so they
// measure the cellular link and not the Wi-Fi between the router and the
// device the panel is open on.

// diagHostRe is what a ping/traceroute target may look like: a hostname or an
// IPv4/IPv6 address. It never starts with "-", so it can't be read as a flag,
// and it carries nothing a shell would interpret.
var diagHostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`)

// runDiag pings or traceroutes host from the router and returns the tool's own
// output. ping exits 1 when nothing answers, which run() already treats as a
// normal result, so an unreachable host still shows its "100% packet loss".
func runDiag(tool, host string) (map[string]any, error) {
	host = strings.TrimSpace(host)
	if !diagHostRe.MatchString(host) {
		return nil, routerErrf("Enter a host name or an IP address, like 8.8.8.8")
	}
	v6 := strings.Contains(host, ":")
	var cmd string
	var timeout time.Duration
	switch tool {
	case "ping":
		bin := "ping"
		if v6 {
			bin = "ping6"
		}
		cmd = fmt.Sprintf("%s -c 5 -W 2 %s 2>&1", bin, host)
		timeout = 20 * time.Second
	case "traceroute":
		bin := "traceroute"
		if v6 {
			bin = "traceroute6"
		}
		cmd = fmt.Sprintf("%s -n -q 1 -w 2 -m 20 %s 2>&1", bin, host)
		timeout = 60 * time.Second
	default:
		return nil, routerErrf("Unknown tool %q (ping or traceroute)", tool)
	}
	out, err := run(cmd, timeout)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"output": strings.TrimRight(out, "\n")}
	if tool == "ping" {
		if m := pingLossRe.FindStringSubmatch(out); m != nil {
			res["loss"], _ = strconv.Atoi(m[1])
		}
		if m := pingAvgRe.FindStringSubmatch(out); m != nil {
			res["avg_ms"], _ = strconv.ParseFloat(m[1], 64)
		}
	}
	return res, nil
}
