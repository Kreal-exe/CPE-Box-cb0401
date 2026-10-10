package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// appVersion is set at build time (-ldflags -X main.appVersion=...): from the
// tag by build.sh for releases, from git describe by fetch.sh/fetch.ps1 when
// start.sh builds from source. "dev" only for a bare go build.
var appVersion = "dev"

//go:embed web
var webFS embed.FS

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func errResp(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(r *http.Request, v any) {
	_ = json.NewDecoder(r.Body).Decode(v)
}

func serveTemplate(w http.ResponseWriter, name string) {
	b, err := webFS.ReadFile(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b = bytes.ReplaceAll(b, []byte("{{v}}"), []byte(assetVersion()))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	_, _ = w.Write(b)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	serveTemplate(w, "web/index.html")
}

// assetVersion is a hash of the embedded web files, used in the asset URLs
// so any rebuild with changed CSS/JS bypasses the browser cache.
var assetVersion = sync.OnceValue(func() string {
	h := sha256.New()
	_ = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := webFS.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
})

// Static CSS/JS for the panel. Public (the login page uses it too) and
// versioned by assetVersion in the page's URLs, so browsers may cache it.
func assetHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "web/assets")
	files := http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	})
}

// guiAddr/guiPort are set in main() from GUI_BIND. webPortUp is true once
// the extra listener on port 80 is running, so the LAN name works without
// a port (http://cpe.box).
var (
	guiAddr, guiPort string
	webPortUp        atomic.Bool
)

func portSuffix() string {
	if webPortUp.Load() {
		return ""
	}
	return ":" + guiPort
}

// listenWebPort also serves the panel on port 80 (GUI_WEB_PORT, "off" to
// disable) of the same interface, so other devices can just type cpe.box.
// macOS and Windows let a normal user bind it; on Linux it needs root or
// CAP_NET_BIND_SERVICE, and without that the panel stays on its own port.
func listenWebPort(h http.Handler) {
	p := getenv("GUI_WEB_PORT", "80")
	if p == "off" || p == "0" || p == guiPort {
		return
	}
	host, _, _ := net.SplitHostPort(guiAddr)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, p))
	if err != nil {
		log.Printf("port %s unavailable (%v) - the panel stays on port %s", p, err, guiPort)
		return
	}
	if p == "80" {
		webPortUp.Store(true)
	}
	go func() { log.Print(http.Serve(ln, h)) }()
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	lanURL := ""
	if ip, err := localIPTowardRouter(); err == nil && !isLoopbackBind(guiAddr) {
		lanURL = "http://" + ip + portSuffix()
	}
	nameURL := ""
	if !isLoopbackBind(guiAddr) {
		nameURL = "http://" + lanHostname() + portSuffix()
	}
	model, firmware := "", ""
	if v, err := cached("model", 10*time.Minute, func() (any, error) { return getRouterModel() }); err == nil {
		m := v.(map[string]string)
		model, firmware = m["model"], m["firmware"]
	}
	ok(w, map[string]any{
		"version":  appVersion,
		"assets":   assetVersion(),
		"model":    model,
		"firmware": firmware,
		"router":   routerIP,
		"name_url": nameURL,
		"lan_url":  lanURL,
		"local":    isLocalClient(r),
	})
}

func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := cached("status", 15*time.Second, func() (any, error) {
		modem, err := getModemConfig()
		var modemOut any = modem
		if err != nil {
			modemOut = map[string]string{"_error": err.Error()}
		}
		return map[string]any{"modem": modemOut, "wifi": getWifiStatus()}, nil
	})
	ok(w, v)
}

func handleSA(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode *int `json:"mode"`
	}
	decodeBody(r, &body)
	if body.Mode == nil {
		errResp(w, fmt.Errorf("missing mode (0=SA+NSA, 1=SA off, 2=force SA, 3=5G off)"))
		return
	}
	cfg, err := setNr5gMode(*body.Mode)
	invalidate("status")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, cfg)
}

func handleSMSC(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Smsc string `json:"smsc"`
		}
		decodeBody(r, &body)
		res, err := setSMSC(body.Smsc)
		if err != nil {
			errResp(w, err)
			return
		}
		ok(w, res)
		return
	}
	res, err := getSMSC()
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, res)
}

func handleSIMNumber(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number string `json:"number"`
	}
	decodeBody(r, &body)
	res, err := setSIMNumber(body.Number)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, res)
}

func handleCellLock(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			LTE     []lteCell `json:"lte"`
			NR      *nrCell   `json:"nr"`
			Persist bool      `json:"persist"`
		}
		decodeBody(r, &body)
		res, err := setCellLock(body.LTE, body.NR, body.Persist)
		invalidate("cellular")
		if err != nil {
			errResp(w, err)
			return
		}
		ok(w, res)
		return
	}
	res, err := getCellLock()
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, res)
}

func handleDiag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tool string `json:"tool"`
		Host string `json:"host"`
	}
	decodeBody(r, &body)
	res, err := runDiag(body.Tool, body.Host)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, res)
}

func handleContacts(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("a") {
	case "save":
		var body struct {
			Index  int    `json:"index"`
			Name   string `json:"name"`
			Number string `json:"number"`
		}
		decodeBody(r, &body)
		c, err := saveContact(body.Index, body.Name, body.Number)
		if err != nil {
			errResp(w, err)
			return
		}
		ok(w, c)
	case "delete":
		var body struct {
			Index   int   `json:"index"`
			Indexes []int `json:"indexes"`
		}
		decodeBody(r, &body)
		if len(body.Indexes) > 0 {
			failed, err := deleteContacts(body.Indexes)
			if err != nil {
				errResp(w, err)
				return
			}
			ok(w, map[string]int{"deleted": len(body.Indexes) - failed, "failed": failed})
			return
		}
		if err := deleteContact(body.Index); err != nil {
			errResp(w, err)
			return
		}
		ok(w, map[string]int{"deleted": 1, "failed": 0})
	case "import":
		var body struct {
			VCF string `json:"vcf"`
		}
		decodeBody(r, &body)
		list := parseVCF(body.VCF)
		if len(list) == 0 {
			errResp(w, fmt.Errorf("No contacts with a phone number found in that file"))
			return
		}
		res, err := importContacts(list)
		if err != nil {
			errResp(w, err)
			return
		}
		ok(w, res)
	case "export":
		v, err := cached("contacts", 30*time.Second, func() (any, error) { return getContacts() })
		if err != nil {
			errResp(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="sim-contacts.vcf"`)
		_, _ = w.Write([]byte(contactsVCF(v.(map[string]any)["contacts"].([]contact))))
	default:
		v, err := cached("contacts", 30*time.Second, func() (any, error) { return getContacts() })
		if err != nil {
			errResp(w, err)
			return
		}
		ok(w, v)
	}
}

func handleBands(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nr5gBand    string `json:"nr5g_band"`
		NsaNr5gBand string `json:"nsa_nr5g_band"`
		LteBand     string `json:"lte_band"`
	}
	decodeBody(r, &body)
	cfg, err := setBands(body.Nr5gBand, body.NsaNr5gBand, body.LteBand)
	invalidate("status", "cellular")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, cfg)
}

func handleWifi(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Band    string `json:"band"`
		Channel any    `json:"channel"`
		Bw      any    `json:"bw"`
	}
	decodeBody(r, &body)
	status, err := setWifi(body.Band, anyToStr(body.Channel), anyToStr(body.Bw))
	invalidate("status")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, status)
}

func anyToStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func handleUciDump(w http.ResponseWriter, r *http.Request) {
	out, err := rawShell("uci show 2>&1")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"output": out})
}

func handleSSHInfo(w http.ResponseWriter, r *http.Request) {
	// The router's old dropbear only offers ssh-rsa (SHA-1) as a host key -
	// a modern OpenSSH client (8.8+) rejects that by default ("no matching
	// host key type found"). It also regenerates that host key on every
	// reboot (ramfs /etc), so a cached known_hosts entry from before the
	// last reboot makes OpenSSH refuse password auth outright ("REMOTE HOST
	// IDENTIFICATION HAS CHANGED"). Without these flags the command may not
	// connect at all, so they always need to be in the hint shown to the user.
	sshCompat := "-o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no"
	ok(w, map[string]string{
		"host":     routerIP,
		"user":     "root",
		"key_path": keyPath,
		"cmd_key":  fmt.Sprintf("ssh %s -i %s root@%s", sshCompat, keyPath, routerIP),
		"cmd_pw":   fmt.Sprintf("ssh %s root@%s", sshCompat, routerIP),
	})
}

func handleReboot(w http.ResponseWriter, r *http.Request) {
	invalidate("model", "status", "cellular", "health", "leds")
	ok(w, rebootRouter())
}

func handleSpoofVersion(w http.ResponseWriter, r *http.Request) {
	reported, err := spoofFirmwareVersion()
	invalidate("model")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"reported": reported})
}

func handleSetRootPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	decodeBody(r, &body)
	if err := setRootPassword(body.Password); err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]bool{"changed": true})
}

func handleWifiScan(w http.ResponseWriter, r *http.Request) {
	band := r.URL.Query().Get("band")
	if band == "" {
		band = "2.4"
	}
	result, err := scanWifiChannels(band)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, result)
}

func handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	result, err := cached("health", 10*time.Second, func() (any, error) { return getSystemHealth() })
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, result)
}

func handleDeviceMonitor(w http.ResponseWriter, r *http.Request) {
	state, err := cached("devices", 20*time.Second, func() (any, error) { return getDeviceMonitorState() })
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, state)
}

func handleDeviceMonitorWhitelist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Macs []string `json:"macs"`
	}
	decodeBody(r, &body)
	state, err := setDeviceWhitelist(body.Macs)
	invalidate("devices")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, state)
}

func handleNotifyConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Backend          string  `json:"backend"`
		TelegramBotToken *string `json:"telegram_bot_token"`
		TelegramChatID   *string `json:"telegram_chat_id"`
		SmsForward       *bool   `json:"sms_forward"`
	}
	decodeBody(r, &body)
	cfg, err := setNotifyConfig(body.Backend, body.TelegramBotToken, body.TelegramChatID, body.SmsForward)
	invalidate("devices")
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, cfg)
}

func handleDataUsage(w http.ResponseWriter, r *http.Request) {
	data, err := cached("usage", 4*time.Second, func() (any, error) { return getDataUsage() })
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, data)
}

func handleCellularInfo(w http.ResponseWriter, r *http.Request) {
	data, err := cached("cellular", 10*time.Second, func() (any, error) { return getCellularInfo() })
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, data)
}

func handleConnectivity(w http.ResponseWriter, r *http.Request) {
	data, err := cached("connectivity", 20*time.Second, func() (any, error) { return checkInternet() })
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, data)
}

func handleLeds(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			On *bool `json:"on"`
		}
		decodeBody(r, &body)
		if body.On == nil {
			errResp(w, fmt.Errorf("missing on (true/false)"))
			return
		}
		res, err := setLeds(*body.On)
		invalidate("leds")
		if err != nil {
			errResp(w, err)
			return
		}
		ok(w, res)
		return
	}
	res, err := cached("leds", 30*time.Second, func() (any, error) { return getLeds() })
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, res)
}

func handleRaw(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cmd string `json:"cmd"`
	}
	decodeBody(r, &body)
	if body.Cmd == "" {
		errResp(w, fmt.Errorf("Empty command"))
		return
	}
	out, err := rawShell(body.Cmd)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"output": out})
}

func handleAT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cmd string `json:"cmd"`
	}
	decodeBody(r, &body)
	// A reboot or bands rewrite could change what mobile.status reports next.
	defer invalidate("cellular")
	out, err := sendAT(body.Cmd, 3*time.Second)
	if err != nil {
		errResp(w, err)
		return
	}
	ok(w, map[string]string{"output": out})
}

func requireMethod(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func loadEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key, val := line[:idx], line[idx+1:]
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// setEnvValue rewrites one KEY=... line in .env (adding it if missing),
// leaving every other line untouched. Used so a change made through the
// GUI (e.g. a new root password) is reflected on disk immediately, not
// just in this process's memory - otherwise the next GUI restart would
// silently fall back to the stale value.
func setEnvValue(key, value string) error {
	existing, err := os.ReadFile(envFilePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	found := false
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !found && strings.HasPrefix(trimmed, key+"=") {
			lines = append(lines, key+"="+value)
			found = true
			continue
		}
		lines = append(lines, line)
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(envFilePath, []byte(content), 0o600)
}

func main() {
	// One-shot CLI actions, run before touching env/net so they work on a
	// fresh binary with nothing else configured. Kept as tiny positional
	// commands rather than a flag library so the CLI surface is
	// self-documenting: cpe-box has one purpose, this is just for setup.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--dump-sms-reader":
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: cpe-box --dump-sms-reader <path>")
				os.Exit(2)
			}
			ok, err := dumpEmbeddedSmsReader(os.Args[2])
			if err != nil {
				fmt.Fprintln(os.Stderr, "dump-sms-reader:", err)
				os.Exit(1)
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "this cpe-box was built without an embedded sms-reader (empty placeholder). Run build.sh to embed one.")
				os.Exit(1)
			}
			return
		case "--version", "-v":
			fmt.Println(appVersion)
			return
		}
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	exeDir := filepath.Dir(exe)
	envFilePath = filepath.Join(exeDir, ".env")
	loadEnvFile(envFilePath)
	keyPath = filepath.Join(exeDir, "router_key")
	// Re-read config that depends on .env, now that it's loaded.
	routerIP = getenv("ROUTER_IP", routerIP)
	routerPass = getenv("ROUTER_ROOT_PASSWORD", routerPass)
	ntfyTopicEnv = getenv("NTFY_TOPIC", ntfyTopicEnv)
	notifyBackendEnv = getenv("NOTIFY_BACKEND", notifyBackendEnv)
	telegramTokenEnv = getenv("TELEGRAM_BOT_TOKEN", telegramTokenEnv)
	telegramChatEnv = getenv("TELEGRAM_CHAT_ID", telegramChatEnv)

	if len(os.Args) > 1 && os.Args[1] == "--provision" {
		if err := provisionRouter(); err != nil {
			log.Fatalf("provision: %v", err)
		}
		return
	}

	// Routers set up by earlier versions have a hook that re-wrote the 5G
	// bands over AT on every reconnect; hand those bands to the stock daemon
	// and swap in the mode-only hook. No-op everywhere else.
	go func() {
		if err := migrateLegacyBandHook(); err != nil {
			log.Printf("legacy band hook migration: %v", err)
		}
	}()

	initSessionSecret()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.Handle("/assets/", assetHandler())
	mux.HandleFunc("/logout", handleLogout)
	mux.HandleFunc("/api/info", handleInfo)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/sa", requireMethod(http.MethodPost, handleSA))
	mux.HandleFunc("/api/smsc", handleSMSC)
	mux.HandleFunc("/api/bands", requireMethod(http.MethodPost, handleBands))
	mux.HandleFunc("/api/sim-number", requireMethod(http.MethodPost, handleSIMNumber))
	mux.HandleFunc("/api/contacts", handleContacts)
	mux.HandleFunc("/api/cell-lock", handleCellLock)
	mux.HandleFunc("/api/diag", requireMethod(http.MethodPost, handleDiag))
	mux.HandleFunc("/api/speedtest", handleSpeedTest)
	mux.HandleFunc("/api/wifi", requireMethod(http.MethodPost, handleWifi))
	mux.HandleFunc("/api/uci-dump", handleUciDump)
	mux.HandleFunc("/api/ssh-info", handleSSHInfo)
	mux.HandleFunc("/api/reboot", requireMethod(http.MethodPost, handleReboot))
	mux.HandleFunc("/api/spoof-version", requireMethod(http.MethodPost, handleSpoofVersion))
	mux.HandleFunc("/api/root-password", requireMethod(http.MethodPost, handleSetRootPassword))
	mux.HandleFunc("/api/wifi-scan", handleWifiScan)
	mux.HandleFunc("/api/system-health", handleSystemHealth)
	mux.HandleFunc("/api/device-monitor", handleDeviceMonitor)
	mux.HandleFunc("/api/device-monitor/whitelist", requireMethod(http.MethodPost, handleDeviceMonitorWhitelist))
	mux.HandleFunc("/api/notify-config", requireMethod(http.MethodPost, handleNotifyConfig))
	mux.HandleFunc("/api/data-usage", handleDataUsage)
	mux.HandleFunc("/api/cellular-info", handleCellularInfo)
	mux.HandleFunc("/api/connectivity", handleConnectivity)
	mux.HandleFunc("/api/leds", handleLeds)
	mux.HandleFunc("/api/stock", handleStock)
	mux.HandleFunc("/api/raw", requireMethod(http.MethodPost, handleRaw))
	mux.HandleFunc("/api/at", requireMethod(http.MethodPost, handleAT))

	// Reachable from every device on the LAN by default (other devices log
	// in with the router's root password - see auth.go). GUI_BIND=127.0.0.1:7777
	// in .env restricts it to this machine.
	guiAddr = getenv("GUI_BIND", "0.0.0.0:7777")
	_, guiPort, _ = net.SplitHostPort(guiAddr)
	if guiPort == "" {
		guiPort = "7777"
	}
	fmt.Println("============================================================")
	fmt.Printf("CPE Box v%s\n", appVersion)
	fmt.Printf("  on this machine:  http://127.0.0.1:%s\n", guiPort)
	handler := withSecurity(mux)
	if !isLoopbackBind(guiAddr) {
		listenWebPort(handler)
		listenTLSPort(handler)
		go keepLanHostnameCurrent()
		fmt.Printf("  on your network:  http://%s%s", lanHostname(), portSuffix())
		if ip, err := localIPTowardRouter(); err == nil {
			fmt.Printf("  (or http://%s%s)", ip, portSuffix())
		}
		fmt.Println()
		fmt.Println("  Other devices log in with the router's root password.")
	}
	fmt.Println("============================================================")
	log.Fatal(http.ListenAndServe(guiAddr, handler))
}
