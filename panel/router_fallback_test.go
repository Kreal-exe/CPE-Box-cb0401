package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The parsers under test call readQCAINFO (AT+QCAINFO over SSH) for the
// carrier list; keep them off any real router so results only depend on the
// fixtures, and give the cached SIM-number lookup a value too.
func TestMain(m *testing.M) {
	readQCAINFO = func() string { return "" }
	cacheData["simnumber"] = cacheEntry{time.Now(), ""}
	os.Exit(m.Run())
}

// Real AT reply block captured from the RG520N-EB modem (LTE + NR5G NSA on
// Telekom.de). The AT fallback must reconstruct the same essentials the stock
// daemon's dump_status would have returned.
const sampleAT = "AT+COPS?\r\n+COPS: 0,0,\"Telekom.de\",13\r\n\r\nOK\r\n" +
	"AT+QNWINFO\r\n+QNWINFO: \"FDD LTE\",\"26201\",\"LTE BAND 3\",1300\r\n" +
	"+QNWINFO: \"FDD NR5G\",\"26201\",\"NR5G BAND 1\",431070\r\n\r\nOK\r\n" +
	"AT+QENG=\"servingcell\"\r\n+QENG: \"servingcell\",\"NOCONN\"\r\n" +
	"+QENG: \"LTE\",\"FDD\",262,01,1929500,321,1300,3,5,5,34BA,-75,-7,-48,25,15,100,-\r\n" +
	"+QENG: \"NR5G-NSA\",262,01,774,-82,30,-11,431070,1,3,0\r\n\r\nOK\r\n" +
	"AT+CPIN?\r\n+CPIN: READY\r\n\r\nOK\r\n" +
	"AT+CEREG?\r\n+CEREG: 0,1\r\n\r\nOK\r\n" +
	"AT+QCCID\r\n+QCCID: 89490200000000000001\r\n\r\nOK\r\n" +
	"AT+CNUM\r\n+CNUM: ,\"+491761234567\",145\r\n\r\nOK\r\n" +
	"AT+CGDCONT?\r\n+CGDCONT: 1,\"IPV4V6\",\"internet.v6.telekom\",\"0.0.0.0\",0,0\r\n\r\nOK\r\n"

func TestParseATCellular(t *testing.T) {
	info := parseATCellular(sampleAT)
	wantStr := map[string]string{
		"operator":     "Telekom.de",
		"network_type": "5G NSA",
		"band_primary": "B3",
		"band_5g":      "n1",
		"earfcn":       "1300",
		"nr_arfcn":     "431070",
		"apn":          "internet.v6.telekom",
		"rsrp":         "-75",
		"rsrq":         "-7",
		"rssi":         "-48",
		"snr":          "25",
		"pci":          "321",
		"rsrp_5g":      "-82",
		"snr_5g":       "30",
		"rsrq_5g":      "-11",
		"pci_5g":       "774",
		"sim_status":   "Ready",
		"sim_number":   "+491761234567",
		"sim_iccid":    "89490200000000000001",
	}
	for k, want := range wantStr {
		if got, _ := info[k].(string); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if info["registered"] != true {
		t.Errorf("registered = %v, want true", info["registered"])
	}
	if info["roaming"] != false {
		t.Errorf("roaming = %v, want false", info["roaming"])
	}
	if info["level"] != 5 { // rsrp -75 -> 5 bars
		t.Errorf("level = %v, want 5", info["level"])
	}
}

// Synthesized NR5G-SA replies for a cb0401 v1 in standalone 5G (no LTE anchor).
// Two firmware shapes are covered: a standalone "NR5G-SA" +QENG line (same field
// order as the NR5G-NSA leg, which is how the real NSA capture reports each RAT),
// and the combined "servingcell",<state>,"NR5G-SA",... form from the Quectel spec.
// Common to both: registration comes from C5GREG (CEREG reads not-registered in
// SA). Field layouts are best-effort — confirm against a live SA unit.
func atSAHeader() string {
	return "AT+COPS?\r\n+COPS: 0,0,\"Telekom.de\",13\r\n\r\nOK\r\n" +
		"AT+QNWINFO\r\n+QNWINFO: \"NR5G-SA\",\"26201\",\"NR5G BAND 78\",633984\r\n\r\nOK\r\n"
}
func atSAFooter() string {
	return "AT+CPIN?\r\n+CPIN: READY\r\n\r\nOK\r\n" +
		"AT+CEREG?\r\n+CEREG: 0,0\r\n\r\nOK\r\n" +
		"AT+C5GREG?\r\n+C5GREG: 0,1\r\n\r\nOK\r\n" +
		"AT+QCCID\r\n+QCCID: 89490200000000000001\r\n\r\nOK\r\n" +
		"AT+CNUM\r\n+CNUM: ,\"+491761234567\",145\r\n\r\nOK\r\n" +
		"AT+CGDCONT?\r\n+CGDCONT: 1,\"IPV4V6\",\"internet\",\"0.0.0.0\",0,0\r\n\r\nOK\r\n"
}

func checkSA(t *testing.T, info map[string]any) {
	t.Helper()
	wantStr := map[string]string{
		"operator": "Telekom.de", "network_type": "5G SA",
		"band_5g": "n78", "band_primary": "",
		"rsrp_5g": "-88", "rsrq_5g": "-11", "snr_5g": "25", "pci_5g": "101", "nr_arfcn": "633984",
		"sim_status": "Ready", "sim_iccid": "89490200000000000001",
	}
	for k, want := range wantStr {
		if got, _ := info[k].(string); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if info["registered"] != true { // from C5GREG, not CEREG
		t.Errorf("registered = %v, want true", info["registered"])
	}
	if info["level"] != 4 { // rsrp_5g -88 -> 4 bars
		t.Errorf("level = %v, want 4", info["level"])
	}
}

func TestParseATCellularSA_standaloneLine(t *testing.T) {
	raw := atSAHeader() +
		"AT+QENG=\"servingcell\"\r\n+QENG: \"servingcell\",\"CONN\"\r\n" +
		"+QENG: \"NR5G-SA\",262,01,101,-88,25,-11,633984,78,1,0\r\n\r\nOK\r\n" +
		atSAFooter()
	checkSA(t, parseATCellular(raw))
}

// Real NR5G-SA capture from a cb0401 v1 (issue #1): combined "servingcell"
// line, and note the serving state reads NOCONN while C5GREG shows registered.
// Also guards that band comes out n78 (not the bogus "n0" a placeholder QCAINFO
// band 0 produced) via the band_5g fallback when there is no LTE anchor.
func TestParseATCellularSA_realV1(t *testing.T) {
	raw := "AT+QNWINFO\r\n+QNWINFO: \"TDD NR5G\",\"20201\",\"NR5G BAND 78\",634080\r\n\r\nOK\r\n" +
		"AT+QENG=\"servingcell\"\r\n" +
		"+QENG: \"servingcell\",\"NOCONN\",\"NR5G-SA\",\"TDD\", 202,01,12AA54086,262,15EC,634080,78,12,-104,-14,19,1,-\r\n\r\nOK\r\n" +
		"AT+C5GREG?\r\n+C5GREG: 0,1\r\n\r\nOK\r\n"
	info := parseATCellular(raw)
	want := map[string]string{
		"network_type": "5G SA", "band_5g": "n78", "band": "n78",
		"pci_5g": "262", "rsrp_5g": "-104", "rsrq_5g": "-14", "snr_5g": "19", "nr_arfcn": "634080",
	}
	for k, v := range want {
		if got, _ := info[k].(string); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if info["registered"] != true { // from C5GREG, not CEREG
		t.Errorf("registered = %v, want true", info["registered"])
	}
	if info["level"] != 2 { // rsrp_5g -104 -> 2 bars
		t.Errorf("level = %v, want 2", info["level"])
	}
}

func TestParseQENGSINR(t *testing.T) {
	// LTE serving line (real NSA capture): ...,-75,-7,-48,25,... -> SINR field 13 = 25.
	if lte, _ := parseQENGSINR(`+QENG: "LTE","FDD",262,01,1929500,321,1300,3,5,5,34BA,-75,-7,-48,25,15,100,-`); lte != "25" {
		t.Errorf("LTE SINR = %q, want 25", lte)
	}
	// NR5G-SA combined line (real cb0401 v1 capture): ...,-92,-11,18,... -> SINR field 13 = 18.
	if _, nr := parseQENGSINR(`+QENG: "servingcell","NOCONN","NR5G-SA","TDD", 202,01,12AA54086,262,15EC,634080,78,7,-92,-11,18,1,-`); nr != "18" {
		t.Errorf("SA SINR = %q, want 18", nr)
	}
	// NR5G-NSA line: SINR is field 4 = 30.
	if _, nr := parseQENGSINR(`+QENG: "NR5G-NSA",262,01,774,-82,30,-11,431070,1,3,0`); nr != "30" {
		t.Errorf("NSA SINR = %q, want 30", nr)
	}
}

func TestNrBandFromArfcn(t *testing.T) {
	// issue #1: 427730 is a live n1 cell the modem labels band 0; the rest
	// spot-check the table and that junk input (out of range, a stray
	// RSRQ-looking value, empty) resolves to 0 rather than a wrong band.
	cases := []struct {
		in   string
		want int
	}{
		{"427730", 1},
		{"431070", 1},
		{"633984", 78},
		{"361000", 3},
		{"185000", 8},
		{"162000", 20},
		{"999999", 0},
		{"0", 0},
		{"", 0},
		{"-11", 0},
	}
	for _, c := range cases {
		if got := nrBandFromArfcn(c.in); got != c.want {
			t.Errorf("nrBandFromArfcn(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// cb0401 v1 (ROM 3.0.116, issue #1) reports the NR band as 0 on a live NSA n1
// cell while the ARFCN (427730) is correct. band_5g must come out n1, not the
// bogus "n0" the raw band field would give.
func TestParseATCellularNSA_band0FromArfcn(t *testing.T) {
	// Real shape of a v1 NSA report: QNWINFO carries a "TDD NR5G" line whose
	// band reads 0, which used to win over the QENG ARFCN derivation and leave
	// band_5g as a bogus "n0". The LTE QNWINFO + QENG lines mirror a live cell.
	raw := "AT+QNWINFO\r\n" +
		"+QNWINFO: \"FDD LTE\",\"26201\",\"LTE BAND 3\",1300\r\n" +
		"+QNWINFO: \"TDD NR5G\",\"26201\",\"NR5G BAND 0\",427730\r\n\r\nOK\r\n" +
		"AT+QENG=\"servingcell\"\r\n" +
		"+QENG: \"servingcell\",\"CONN\"\r\n" +
		"+QENG: \"LTE\",\"FDD\",262,01,1929500,321,1300,3,5,5,34BA,-75,-7,-48,25,15,100,-\r\n" +
		"+QENG: \"NR5G-NSA\",262,01,0,-85,25,-11,427730,0,3,0\r\n\r\nOK\r\n"
	info := parseATCellular(raw)
	if got, _ := info["band_5g"].(string); got != "n1" {
		t.Errorf("band_5g = %q, want n1", got)
	}
	if got, _ := info["nr_arfcn"].(string); got != "427730" {
		t.Errorf("nr_arfcn = %q, want 427730", got)
	}
	if got, _ := info["network_type"].(string); got != "5G NSA" {
		t.Errorf("network_type = %q, want 5G NSA", got)
	}
	// With no live QCAINFO (aggregatedBands empty here), the Carriers row is
	// composed from the legs and must carry the derived 5G band.
	if got, _ := info["band"].(string); !strings.Contains(got, "n1") {
		t.Errorf("band = %q, want it to contain n1", got)
	}
}

// cb0401 v1 (RG520N-EB, ROM 3.0.116) in NSA reports PCI 0 for the NR leg in
// QENG (issue #4). The NR measurement report has the real PCI for the same
// ARFCN; the Connection card must show it, as the Cells-around card does.
func TestParseATCellularNSA_pci0FromMeasReport(t *testing.T) {
	raw := "AT+QENG=\"servingcell\"\r\n" +
		"+QENG: \"servingcell\",\"NOCONN\"\r\n" +
		"+QENG: \"LTE\",\"FDD\",219,10,7EA03,350,1850,3,5,5,CA,-79,-13,-47,15,11,-150,-\r\n" +
		"+QENG: \"NR5G-NSA\",219,10,0,-92,200,-11,647040,0,0,1\r\n\r\nOK\r\n" +
		"AT+QNWCFG=\"nr5g_meas_info\"\r\n" +
		"+QNWCFG: \"nr5g_meas_info\",1,647040,74,-92,-11,647040,75,-101,-14\r\n\r\nOK\r\n"
	info := parseATCellular(raw)
	if got, _ := info["pci_5g"].(string); got != "74" {
		t.Errorf("pci_5g = %q, want 74", got)
	}
	if got, _ := info["band_5g"].(string); got != "n78" {
		t.Errorf("band_5g = %q, want n78", got)
	}
	if got, _ := info["snr_5g"].(string); got != "20" {
		t.Errorf("snr_5g = %q, want 20", got)
	}

	// Report off (or no row for this ARFCN): keep what QENG said.
	off := strings.Replace(raw, `"nr5g_meas_info",1,647040,74,-92,-11,647040,75,-101,-14`, `"nr5g_meas_info",0`, 1)
	if got, _ := parseATCellular(off)["pci_5g"].(string); got != "0" {
		t.Errorf("report off: pci_5g = %q, want 0", got)
	}
}

func TestNormalizeNrSinr(t *testing.T) {
	cases := map[string]string{
		"195":  "19.5", // cb0401 v1 reports NR SINR in 0.1 dB
		"230":  "23",
		"-420": "-42",
		"18":   "18", // already dB - unchanged
		"30":   "30",
		"0":    "0",
		"40":   "40",
		"-":    "-", // placeholder, left as-is
		"":     "",
	}
	for in, want := range cases {
		if got := normalizeNrSinr(in); got != want {
			t.Errorf("normalizeNrSinr(%q) = %q, want %q", in, got, want)
		}
	}
}

// Real dump_status from the stock daemon on cb0401 v2 (ROM 3.0.57). Signal
// fields are quoted strings and ci_5g is the "-" placeholder that used to
// break a json.Number parse — parseDumpStatus (via loose) must handle both.
const sampleDump = `{"sim":{"status":1,"pin_remains":3,"puk_remains":10,"lock":1,"iccid":"89490200000000000001","imsi":"262010000000001","number":"+491761234567","country":"DE"},"status":{"registration":1,"isp":"Telekom.de","apn":"internet.v6.telekom","rat":"5G NSA","band":"B3+B8+n1","roam":0,"cell_band":"B3(FDD 1800+)","ci":"26383616","pci":"321","level":4,"rsrq":"-7.0","rsrp":"-75.4","snr":"28.0","rssi":"-48.4","cell_band_5g":"n1(FDD 2100)","ci_5g":"-","pci_5g":"774","rsrq_5g":"-11.0","rsrp_5g":"-83.0","snr_5g":"28.0"},"code":0}`

func TestParseDumpStatus(t *testing.T) {
	info, err := parseDumpStatus(sampleDump)
	if err != nil {
		t.Fatalf("parseDumpStatus errored: %v", err)
	}
	want := map[string]any{
		"operator": "Telekom.de", "network_type": "5G NSA",
		"rsrp": "-75.4", "rsrp_5g": "-83.0", "pci": "321", "pci_5g": "774",
		"sim_status": "Ready", "level": 4, "registered": true, "roaming": false,
		"sim_locked": true,
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("%s = %v, want %v", k, info[k], v)
		}
	}
}

// loose must accept a quoted string, a bare number, and the "-" placeholder
// without ever failing the surrounding parse.
func TestLooseUnmarshal(t *testing.T) {
	cases := map[string]string{
		`"-77.0"`: "-77.0",
		`-77.0`:   "-77.0",
		`-77`:     "-77",
		`"-"`:     "-",
		`""`:      "",
		`null`:    "",
	}
	for in, want := range cases {
		var l loose
		if err := l.UnmarshalJSON([]byte(in)); err != nil {
			t.Errorf("UnmarshalJSON(%s) errored: %v", in, err)
			continue
		}
		if l.String() != want {
			t.Errorf("UnmarshalJSON(%s) = %q, want %q", in, l.String(), want)
		}
	}
}

// The stock UI's "4G only" writes mode_pref=LTE and leaves nr5g_disable_mode
// at 0; the panel must read that back as LTE only, not SA+NSA.
func TestPanelMode(t *testing.T) {
	cases := []struct{ pref, disable, want string }{
		{"LTE:NR5G", "0", "0"},
		{"LTE:NR5G", "1", "1"},
		{"LTE:NR5G", "2", "2"},
		{"LTE", "0", "3"},
		{"NR5G", "2", "2"},
		{"", "1", "1"},
	}
	for _, c := range cases {
		if got := panelMode(c.pref, c.disable); got != c.want {
			t.Errorf("panelMode(%q, %q) = %q, want %q", c.pref, c.disable, got, c.want)
		}
	}
	if cmds := modeATCommands(3); len(cmds) != 1 || !strings.Contains(cmds[0], `"mode_pref",LTE`) {
		t.Errorf("LTE only must go through mode_pref, got %v", cmds)
	}
}

// Live rate from two /proc/net/dev samples one second apart, captured during a
// ~35 MB/s download on cb0401 v2 (rmnet_mhi0 had just wrapped its 32-bit rx).
func TestNetdevRate(t *testing.T) {
	lines := []string{
		"NETDEV1  wwan0: 281593822828 226408488 0 0 0 0 0 0 24273869712 53909517 0 0 0 0 0 0",
		"NETDEV1 rmnet_mhi0: 4294000000 37131993 0 0 0 0 0 0 3118322017 53831586 0 0 0 0 0 0",
		"NETDEV2  wwan0: 281628622828 226438488 0 0 0 0 0 0 24274069712 53919517 0 0 0 0 0 0",
		"NETDEV2 rmnet_mhi0: 33832704 37161993 0 0 0 0 0 0 3118522017 53841586 0 0 0 0 0 0",
	}
	rx, tx, ok := netdevRate(lines)
	if !ok || rx != 34800000 || tx != 200000 {
		t.Errorf("netdevRate = %d, %d, %v; want 34800000, 200000, true", rx, tx, ok)
	}
	if _, _, ok := netdevRate(nil); ok {
		t.Error("netdevRate with no samples should report !ok")
	}
}

// Real LTE-only reply (mode_pref=LTE) on cb0401 v2: the LTE fields sit in the
// servingcell line itself instead of a separate +QENG: "LTE" line.
func TestLTEOnlyQENG(t *testing.T) {
	raw := "AT+QENG=\"servingcell\"\r\n+QENG: \"servingcell\",\"NOCONN\",\"LTE\",\"FDD\",262,01,1929500,321,1300,3,5,5,34BA,-72,-8,-46,25,15,-290,-\r\n\r\nOK\r\n"
	lte, nr := parseQENGSINR(raw)
	if lte != "25" || nr != "" {
		t.Errorf("parseQENGSINR = %q, %q; want 25, \"\"", lte, nr)
	}
	f := lteQENGFields(raw)
	if len(f) < 14 || f[4] != "321" || f[5] != "1300" || f[10] != "-72" {
		t.Errorf("lteQENGFields = %v", f)
	}
}

// Real AT+QCAINFO in NSA: the n1 SCC has scell_state 1 and RSRP 0 - configured
// but inactive, and the stock daemon reports B3+B8+n78 at the same moment.
func TestQcaInactive(t *testing.T) {
	cases := map[string]bool{
		",1,450,0,-,-":               true,
		",284":                       false,
		",2,225,-97,-10,-70,4,0,-,-": false,
		",2,23,-75,-7,-52,30,0,-,-":  false,
	}
	for tail, want := range cases {
		if got := qcaInactive(tail); got != want {
			t.Errorf("qcaInactive(%q) = %v, want %v", tail, got, want)
		}
	}
}

// alen0038's issue #3 capture: AT+CGPADDR returns IPv6 as 16 decimal octets.
func TestParseCGPADDR(t *testing.T) {
	raw := "+CGPADDR: 1,\"42.1.5.153.2.6.50.201.235.23.86.117.149.80.113.211\"\n" +
		"+CGPADDR: 2,\"42.1.5.159.216.24.19.117.149.208.106.196.24.161.190.155\"\n" +
		"+CGPADDR: 3,\"0.0.0.0\",\"0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0\"\n"
	v4, v6 := parseCGPADDR(raw)
	if v4 != "" {
		t.Errorf("v4 = %q, want empty (IPv6-only context)", v4)
	}
	if v6 != "2a01:599:206:32c9:eb17:5675:9550:71d3" {
		t.Errorf("v6 = %q", v6)
	}
	// Dual-stack row with a real v4
	raw2 := `+CGPADDR: 1,"10.188.252.14","42.1.5.153.2.6.50.201.235.23.86.117.149.80.113.211"`
	v4, v6 = parseCGPADDR(raw2)
	if v4 != "10.188.252.14" || v6 != "2a01:599:206:32c9:eb17:5675:9550:71d3" {
		t.Errorf("dual = %q / %q", v4, v6)
	}
	// All-zero only
	if a, b := parseCGPADDR(`+CGPADDR: 1,"0.0.0.0"`); a != "" || b != "" {
		t.Errorf("all-zero = %q/%q", a, b)
	}
}
