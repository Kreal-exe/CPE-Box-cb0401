package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ------------------------------------------------------------------ HTTPS ---
// Chrome's "Always use secure connections" (on by default for more and more
// people) tries https://cpe.box first and, with nobody on 443, shows a full-
// page "This site doesn't support a secure connection" warning before it
// falls back to HTTP. So the panel also serves itself on 443 with its own
// certificate. The certificate is self-signed - there's no CA for a LAN name
// - so the browser warns once about it; accept it and it's remembered. The
// key pair is generated the first time and kept next to .env, so the warning
// doesn't come back after every restart.

var tlsKeyPaths = func() (cert, key string) {
	return filepath.Join(filepath.Dir(envFilePath), "panel_cert.pem"), filepath.Join(filepath.Dir(envFilePath), "panel_key.pem")
}

// panelCertificate loads the saved certificate, or makes a new one covering
// the LAN names and this machine's address toward the router.
func panelCertificate() (tls.Certificate, error) {
	certPath, keyPath := tlsKeyPaths()
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, perr := x509.ParseCertificate(c.Certificate[0]); perr == nil && time.Now().Before(leaf.NotAfter.AddDate(0, -1, 0)) {
			return c, nil
		}
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: lanHostname(), Organization: []string{"CPE Box"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              append(lanHostnames(), "localhost"),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if ip, err := localIPTowardRouter(); err == nil {
		if p := net.ParseIP(ip); p != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, p)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDer, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})
	_ = os.WriteFile(certPath, certPem, 0o644)
	_ = os.WriteFile(keyPath, keyPem, 0o600)
	return tls.X509KeyPair(certPem, keyPem)
}

// listenTLSPort serves the panel over HTTPS on GUI_TLS_PORT (443; "off" to
// disable) of the same interface. Like port 80 it's best-effort: without the
// right to bind it the panel just stays on HTTP.
func listenTLSPort(h http.Handler) {
	p := getenv("GUI_TLS_PORT", "443")
	if p == "off" || p == "0" {
		return
	}
	cert, err := panelCertificate()
	if err != nil {
		log.Printf("https: no certificate (%v) - the panel stays on http", err)
		return
	}
	host, _, _ := net.SplitHostPort(guiAddr)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, p))
	if err != nil {
		log.Printf("port %s unavailable (%v) - https is off, the panel stays on http", p, err)
		return
	}
	srv := &http.Server{Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	go func() { log.Print(srv.ServeTLS(ln, "", "")) }()
}
