package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The panel can run a root shell on the router, so anyone reaching it from
// another device has to log in with the router's root password. Requests
// from this machine itself skip that - unless they arrive through a reverse
// proxy/tunnel (cloudflared connects from localhost too), which is treated
// as remote.

const (
	sessionCookie = "cpebox_session"
	sessionTTL    = 30 * 24 * time.Hour
	csrfHeader    = "X-CPE-Box"
)

var sessionSecret []byte

func initSessionSecret() {
	if s, err := hex.DecodeString(getenv("GUI_SESSION_SECRET", "")); err == nil && len(s) >= 32 {
		sessionSecret = s
		return
	}
	sessionSecret = make([]byte, 32)
	_, _ = rand.Read(sessionSecret)
	// Persisted so logins survive a GUI restart; best-effort.
	_ = setEnvValue("GUI_SESSION_SECRET", hex.EncodeToString(sessionSecret))
}

func sign(payload string) string {
	m := hmac.New(sha256.New, sessionSecret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// A session is bound to the password it was issued under, so changing the
// root password logs every other device out.
func newSessionValue() string {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	return exp + "." + sign(exp+"|"+routerPass)
}

func validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, mac, found := strings.Cut(c.Value, ".")
	if !found {
		return false
	}
	t, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > t {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(sign(exp+"|"+routerPass)))
}

func isLocalClient(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Failed logins are throttled per client address.
var (
	loginMu       sync.Mutex
	loginFailures = map[string][]time.Time{}
)

func loginBlocked(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	var recent []time.Time
	for _, t := range loginFailures[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	loginFailures[ip] = recent
	return len(recent) >= 10
}

func recordLoginFailure(ip string) {
	loginMu.Lock()
	loginFailures[ip] = append(loginFailures[ip], time.Now())
	loginMu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		serveTemplate(w, "web/login.html")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	if loginBlocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "Too many attempts - try again in a few minutes"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	decodeBody(r, &body)
	if routerPass == "" || subtle.ConstantTimeCompare([]byte(body.Password), []byte(routerPass)) != 1 {
		recordLoginFailure(ip)
		time.Sleep(time.Second)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Wrong password"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    newSessionValue(),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil, // https gets a Secure cookie; plain http must still work
		SameSite: http.SameSiteStrictMode,
	})
	ok(w, map[string]bool{"logged_in": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// allowedHost rejects DNS-rebinding: a hostile site that re-points its own
// name at this machine would pass the same-origin check, but not this one.
// Only IP literals, localhost and the panel's own LAN name are served.
func allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if net.ParseIP(host) != nil || host == "localhost" {
		return true
	}
	for _, h := range lanHostnames() {
		if host == h {
			return true
		}
	}
	return false
}

// withSecurity gates every request: login for remote clients, and for all
// state-changing API calls a custom header plus a same-origin check - a
// custom header can't be sent cross-site without a CORS preflight (which
// this server never grants), so a malicious web page can't drive the API,
// not even against 127.0.0.1.
func withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		local := isLocalClient(r)
		authed := local || validSession(r)

		if strings.HasPrefix(r.URL.Path, "/assets/") && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/login" {
			if authed && r.Method == http.MethodGet {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			if r.Method == http.MethodPost && r.Header.Get(csrfHeader) == "" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			handleLogin(w, r)
			return
		}
		if !authed {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Login required", "login": true})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Header.Get(csrfHeader) == "" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
