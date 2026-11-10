package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------ cell lock ---
// AT+QNWLOCK pins the modem to specific cells: LTE to up to 10 EARFCN/PCI
// pairs, 5G to one PCI/ARFCN/SCS/band. "save_ctrl" decides whether a lock
// survives a reboot (kept in the modem's NV, so nothing on the router side has
// to re-apply it). Syntax confirmed with AT+QNWLOCK=? on the RG520N.
//
// The cells the modem is on come from AT+QCAINFO, not from the QENG serving
// line: cb0401 v1 firmware reports PCI 0 and band 0 for the NR leg in QENG
// (issue #1), while QCAINFO carries the real PCI and ARFCN of every carrier.
// Neighbours come from AT+QENG="neighbourcell" (LTE) and the modem's NR
// measurement report (AT+QNWCFG="nr5g_meas_info"), which is off by default and
// switched on the first time the list is read.

var (
	qnwlock4gRe   = regexp.MustCompile(`\+QNWLOCK:\s*"common/4g",(\d+)((?:,\d+,\d+)*)`)
	qnwlock5gRe   = regexp.MustCompile(`\+QNWLOCK:\s*"common/5g",(\d+)(?:,(\d+),(\d+),(\d+))?`)
	qnwlockSaveRe = regexp.MustCompile(`\+QNWLOCK:\s*"save_ctrl",(\d),(\d)`)
	qcainfoRe     = regexp.MustCompile(`\+QCAINFO:\s*"(PCC|SCC)",([^\r\n]+)`)
	neighLTERe    = regexp.MustCompile(`\+QENG:\s*"neighbourcell (intra|inter)","LTE",([^\r\n]+)`)
	nrMeasRe      = regexp.MustCompile(`\+QNWCFG:\s*"nr5g_meas_info",(\d)((?:,[-\d]+)*)`)
)

type lteCell struct {
	Earfcn int `json:"earfcn"`
	PCI    int `json:"pci"`
}

type nrCell struct {
	PCI   int `json:"pci"`
	Arfcn int `json:"arfcn"`
	SCS   int `json:"scs"` // kHz: 15, 30, 60, 120, 240
	Band  int `json:"band"`
}

// radioCell is one carrier or neighbour as the page lists it. Signal values
// are strings because the modem reports "-" for the ones it doesn't measure.
type radioCell struct {
	RAT     string `json:"rat"`  // LTE or NR
	Role    string `json:"role"` // PCC, SCC, neighbour
	Band    string `json:"band"` // B3, n78
	Arfcn   int    `json:"arfcn"`
	PCI     int    `json:"pci"`
	RSRP    string `json:"rsrp,omitempty"`
	RSRQ    string `json:"rsrq,omitempty"`
	RSSI    string `json:"rssi,omitempty"`
	SINR    string `json:"sinr,omitempty"`
	Serving bool   `json:"serving"`
	Key     string `json:"key"` // for nicknames: lte:1300:321
}

func cellKey(rat string, arfcn, pci int) string {
	return fmt.Sprintf("%s:%d:%d", strings.ToLower(rat), arfcn, pci)
}

// nrSCS maps the QENG <scs> code to kHz, the unit QNWLOCK takes.
var nrSCS = map[string]int{"0": 15, "1": 30, "2": 60, "3": 120, "4": 240}

// lteSignalsPlausible: cb0401 v1 SCC lines in 3-CA sometimes come back with
// physically impossible values (seen: RSRP=-14, RSRQ=49). RSRP for LTE lives
// in -140..-44 dBm per 3GPP TS 36.133, RSRQ in -34..0 dB. Reject anything
// well outside those - the SCC is real but its signal column is garbage.
func lteSignalsPlausible(rsrp, rsrq string) bool {
	if v, err := strconv.ParseFloat(strings.TrimSpace(rsrp), 64); err == nil && (v > -40 || v < -145) {
		return false
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(rsrq), 64); err == nil && (v > 0 || v < -35) {
		return false
	}
	return true
}

// lteEarfcnBands: downlink EARFCN ranges per LTE band (3GPP TS 36.101).
var lteEarfcnBands = []struct{ lo, hi, band int }{
	{0, 599, 1}, {600, 1199, 2}, {1200, 1949, 3}, {1950, 2399, 4}, {2400, 2649, 5},
	{2750, 3449, 7}, {3450, 3799, 8}, {5010, 5179, 12}, {5180, 5279, 13}, {5280, 5379, 14},
	{5730, 5849, 17}, {5850, 5999, 18}, {6000, 6149, 19}, {6150, 6449, 20}, {6450, 6599, 21},
	{8040, 8689, 25}, {8690, 9039, 26}, {9210, 9659, 28}, {9660, 9769, 29}, {9770, 9869, 30},
	{9920, 10359, 32}, {36200, 36349, 34}, {37750, 38249, 38}, {38250, 38649, 39},
	{38650, 39649, 40}, {39650, 41589, 41}, {41590, 43589, 42}, {43590, 45589, 43},
	{46790, 54539, 46}, {55240, 56739, 48}, {66436, 67335, 66}, {68586, 68935, 71},
}

func lteBandFromEarfcn(e int) string {
	for _, r := range lteEarfcnBands {
		if e >= r.lo && e <= r.hi {
			return "B" + strconv.Itoa(r.band)
		}
	}
	return ""
}

func getCellLock() (map[string]any, error) {
	raw, err := atQuery([]string{
		`AT+QNWLOCK="common/4g"`,
		`AT+QNWLOCK="common/5g"`,
		`AT+QNWLOCK="save_ctrl"`,
		`AT+QENG="servingcell"`,
		`AT+QCAINFO`,
		`AT+QENG="neighbourcell"`,
		`AT+QNWCFG="nr5g_meas_info"`,
	}, 400*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if m := nrMeasRe.FindStringSubmatch(raw); m != nil && m[1] == "0" {
		// NR neighbours are only reported with the measurement report on;
		// it's a reporting switch, it doesn't change how the modem connects.
		if more, err := atQuery([]string{`AT+QNWCFG="nr5g_meas_info",1`, `AT+QNWCFG="nr5g_meas_info"`}, 600*time.Millisecond); err == nil {
			raw += "\n" + more
		}
	}
	res := parseCellLock(raw)
	res["names"] = cellNames()
	return res, nil
}

// nrPCIFromMeas returns the PCI the NR measurement report lists for arfcn, or
// 0. v1 firmware reports PCI 0 for a live NR cell in QENG/QCAINFO; the report
// (arfcn,pci,rsrp,rsrq per cell, serving cell first) has the real one.
func nrPCIFromMeas(raw string, arfcn int) int {
	if arfcn <= 0 {
		return 0
	}
	m := nrMeasRe.FindStringSubmatch(raw)
	if m == nil {
		return 0
	}
	fs := strings.Split(strings.TrimPrefix(m[2], ","), ",")
	for i := 0; i+1 < len(fs); i += 4 {
		if a, ok := atoiDash(fs[i]); ok && a == arfcn {
			if p, ok := atoiDash(fs[i+1]); ok && p > 0 {
				return p
			}
		}
	}
	return 0
}

func atoiDash(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

// parseCellLock reads the lock state, the serving cell(s), the carriers and
// the neighbours out of one AT reply block.
func parseCellLock(raw string) map[string]any {
	res := map[string]any{"lte": []lteCell{}, "nr": nil, "persist": false}
	if m := qnwlock4gRe.FindStringSubmatch(raw); m != nil {
		var cells []lteCell
		nums := strings.Split(strings.TrimPrefix(m[2], ","), ",")
		for i := 0; i+1 < len(nums); i += 2 {
			e, _ := strconv.Atoi(nums[i])
			p, _ := strconv.Atoi(nums[i+1])
			cells = append(cells, lteCell{e, p})
		}
		if cells != nil {
			res["lte"] = cells
		}
	}
	if m := qnwlock5gRe.FindStringSubmatch(raw); m != nil && m[2] != "" {
		pci, _ := strconv.Atoi(m[1])
		arfcn, _ := strconv.Atoi(m[2])
		scs, _ := strconv.Atoi(m[3])
		band, _ := strconv.Atoi(m[4])
		res["nr"] = nrCell{pci, arfcn, scs, band}
	}
	if m := qnwlockSaveRe.FindStringSubmatch(raw); m != nil {
		res["persist"] = m[1] == "1" || m[2] == "1"
	}

	// QENG: the SCS the NR leg uses (QCAINFO doesn't carry it), and the
	// serving cells as a fallback when QCAINFO doesn't answer.
	scs := 0
	var qengLTE *lteCell
	var qengNR []string // pci, arfcn, band
	if f := lteQENGFields(raw); len(f) >= 7 {
		if e, ok := atoiDash(f[5]); ok {
			if p, ok := atoiDash(f[4]); ok {
				qengLTE = &lteCell{e, p}
			}
		}
	}
	// NR fields for a synthetic carrier when QCAINFO has none: pci, arfcn, band, rsrp, sinr, rsrq.
	if f := qengFields(raw, `"NR5G-NSA",`); len(f) >= 10 {
		qengNR, scs = []string{f[2], f[6], f[7], f[3], f[4], f[5]}, nrSCS[f[9]]
	} else if f := qengFields(raw, `"NR5G-SA",`); len(f) >= 10 {
		qengNR, scs = []string{f[2], f[6], f[7], f[3], f[4], f[5]}, nrSCS[f[9]]
	} else if f := qengFields(raw, `"servingcell",`); len(f) >= 15 && f[1] == "NR5G-SA" {
		qengNR, scs = []string{f[6], f[8], f[9], f[11], f[13], f[12]}, nrSCS[f[14]]
	}

	// Carriers from QCAINFO. LTE: earfcn,bw,"LTE BAND n",state,pci,rsrp,rsrq,
	// rssi,sinr,... NR: arfcn,bw,"NR5G BAND n",pci[,rsrp,rsrq,sinr] (NSA legs
	// carry no signal values).
	var carriers []radioCell
	for _, m := range qcainfoRe.FindAllStringSubmatch(raw, -1) {
		f := strings.Split(m[2], ",")
		for i := range f {
			f[i] = strings.Trim(strings.TrimSpace(f[i]), `"`)
		}
		if len(f) < 4 {
			continue
		}
		arfcn, ok := atoiDash(f[0])
		if !ok {
			continue
		}
		c := radioCell{Role: m[1], Arfcn: arfcn}
		bandNum := 0
		if bm := bandNumRe.FindStringSubmatch(f[2]); bm != nil {
			bandNum, _ = strconv.Atoi(bm[1])
		}
		if strings.Contains(f[2], "NR5G") {
			// cb0401 v1 writes a placeholder "SCC",0,0,"NR5G BAND 0",0 for the
			// NR leg when no QCAINFO value is really available (the serving-cell
			// path has it instead). Skip that placeholder so it doesn't show up
			// as a bogus "5G cell 0 n0".
			if arfcn == 0 {
				continue
			}
			c.RAT = "NR"
			if bandNum == 0 {
				bandNum = nrBandFromArfcn(f[0])
			}
			if bandNum == 0 {
				continue // can't label the band and the ARFCN already looked suspicious
			}
			c.Band = "n" + strconv.Itoa(bandNum)
			// Two NR shapes exist: short "arfcn,bw,"band",pci" (4 fields,
			// NSA leg - no signal values) and long "arfcn,bw,"band",state,
			// pci,rsrp,rsrq,sinr,..." (8+ fields, configured SCC). The long
			// form fits the LTE layout; take PCI and the signal values from
			// the right indexes rather than hard-coding one shape.
			if len(f) >= 8 {
				if c.PCI, ok = atoiDash(f[4]); !ok {
					continue
				}
				c.RSRP, c.RSRQ, c.RSSI, c.SINR = f[5], f[6], f[7], ""
				if len(f) >= 9 {
					c.SINR = f[8]
				}
				if m[1] == "SCC" && (c.SINR == "0" || c.SINR == "0.0") {
					c.SINR = ""
				}
				if qcaInactive(strings.Join(f[5:], ",")) {
					continue
				}
			} else {
				if c.PCI, ok = atoiDash(f[3]); !ok {
					continue
				}
			}
		} else {
			c.RAT = "LTE"
			c.Band = "B" + strconv.Itoa(bandNum)
			if len(f) < 5 {
				continue
			}
			if c.PCI, ok = atoiDash(f[4]); !ok {
				continue
			}
			if len(f) >= 9 {
				c.RSRP, c.RSRQ, c.RSSI, c.SINR = f[5], f[6], f[7], f[8]
			}
			// On an SCC the modem reports "0" for SINR it didn't measure,
			// not a real 0 dB reading - hide it rather than show a bogus value.
			if m[1] == "SCC" && (c.SINR == "0" || c.SINR == "0.0") {
				c.SINR = ""
			}
			// 3-CA on cb0401 v1 can return impossible RSRP/RSRQ for an SCC
			// (e.g. RSRP=-14, RSRQ=49). Keep the row (the CA is real), drop
			// the garbage column.
			if m[1] == "SCC" && !lteSignalsPlausible(c.RSRP, c.RSRQ) {
				c.RSRP, c.RSRQ, c.RSSI, c.SINR = "", "", "", ""
			}
			if qcaInactive(strings.Join(f[5:], ",")) {
				continue // a configured SCC with no signal isn't in use
			}
		}
		c.Serving = true
		c.Key = cellKey(c.RAT, c.Arfcn, c.PCI)
		carriers = append(carriers, c)
	}
	// If QCAINFO had no NR row but QENG shows a live NR5G-NSA/SA leg (v1 writes
	// a placeholder "SCC",0,0,"NR5G BAND 0",0 for the NR carrier), synthesize a
	// carrier from the QENG line so the Cells-around card and the Nearby filter
	// know about it. Signal values come from QENG; PCI falls back to the NR
	// measurement report when QENG also reports 0.
	hasNR := false
	for _, c := range carriers {
		if c.RAT == "NR" {
			hasNR = true
			break
		}
	}
	if !hasNR && qengNR != nil {
		pci, ok1 := atoiDash(qengNR[0])
		arfcn, ok2 := atoiDash(qengNR[1])
		band, _ := atoiDash(qengNR[2])
		if band == 0 {
			band = nrBandFromArfcn(qengNR[1])
		}
		if pci == 0 {
			pci = nrPCIFromMeas(raw, arfcn)
		}
		if ok1 && ok2 && band > 0 && arfcn > 0 {
			c := radioCell{RAT: "NR", Role: "PCC", Band: "n" + strconv.Itoa(band), Arfcn: arfcn, PCI: pci, Serving: true}
			c.RSRP, c.SINR, c.RSRQ = qengNR[3], normalizeNrSinr(qengNR[4]), qengNR[5]
			c.Key = cellKey(c.RAT, c.Arfcn, c.PCI)
			carriers = append(carriers, c)
		}
	}
	if carriers == nil {
		carriers = []radioCell{}
	}
	res["carriers"] = carriers

	// Serving cells in the fields QNWLOCK wants: the LTE PCC (the anchor) and
	// the NR carrier, from QCAINFO first.
	for _, c := range carriers {
		if c.RAT == "LTE" && c.Role == "PCC" && res["serving_lte"] == nil {
			res["serving_lte"] = lteCell{c.Arfcn, c.PCI}
		}
		if c.RAT == "NR" && res["serving_nr"] == nil && scs > 0 && c.PCI > 0 {
			band, _ := strconv.Atoi(strings.TrimPrefix(c.Band, "n"))
			if band > 0 {
				res["serving_nr"] = nrCell{c.PCI, c.Arfcn, scs, band}
			}
		}
	}
	if res["serving_lte"] == nil && qengLTE != nil {
		res["serving_lte"] = *qengLTE
	}
	if res["serving_nr"] == nil && qengNR != nil && scs > 0 {
		pci, ok1 := atoiDash(qengNR[0])
		arfcn, ok2 := atoiDash(qengNR[1])
		band, _ := atoiDash(qengNR[2])
		if band == 0 {
			band = nrBandFromArfcn(qengNR[1])
		}
		// v1 firmware sometimes reports PCI 0 for a live NR cell; the NR
		// measurement report has the real one and is already in raw.
		if pci == 0 {
			pci = nrPCIFromMeas(raw, arfcn)
		}
		if ok1 && ok2 && band > 0 && pci > 0 {
			res["serving_nr"] = nrCell{pci, arfcn, scs, band}
		}
	}

	// Neighbours. LTE: earfcn,pci,rsrq,rsrp,rssi,sinr,... (RSRQ before RSRP).
	// The serving cell shows up in the intra list too; it's marked, not dropped.
	// Mark serving cells by (RAT, ARFCN) too, not just exact Key. On v1 the
	// serving NR has PCI 0 in QCAINFO, but the NR measurement report returns
	// the real PCI - different keys, same cell. Treating "same RAT + same
	// ARFCN" as serving stops the connected cell showing up as a neighbour.
	isServing := map[string]bool{}
	servingNRArfcn := map[int]bool{}
	for _, c := range carriers {
		isServing[c.Key] = true
		if c.RAT == "NR" {
			servingNRArfcn[c.Arfcn] = true
		}
	}
	serving := func(rat string, arfcn, pci int) bool {
		if isServing[cellKey(rat, arfcn, pci)] {
			return true
		}
		// Same NR carrier the modem is on but a different PCI reported by the
		// measurement report (v1 QCAINFO can give PCI 0) is still the serving cell.
		return rat == "NR" && servingNRArfcn[arfcn]
	}
	var neigh []radioCell
	seen := map[string]bool{}
	for _, m := range neighLTERe.FindAllStringSubmatch(raw, -1) {
		f := strings.Split(m[2], ",")
		if len(f) < 5 {
			continue
		}
		e, ok1 := atoiDash(f[0])
		p, ok2 := atoiDash(f[1])
		if !ok1 || !ok2 {
			continue
		}
		c := radioCell{RAT: "LTE", Role: "neighbour", Band: lteBandFromEarfcn(e), Arfcn: e, PCI: p,
			RSRQ: strings.TrimSpace(f[2]), RSRP: strings.TrimSpace(f[3]), RSSI: strings.TrimSpace(f[4])}
		if len(f) > 5 && strings.TrimSpace(f[5]) != "-" {
			c.SINR = strings.TrimSpace(f[5])
		}
		c.Key = cellKey("LTE", e, p)
		c.Serving = serving(c.RAT, c.Arfcn, c.PCI)
		if !seen[c.Key] {
			seen[c.Key] = true
			neigh = append(neigh, c)
		}
	}
	// NR: the measurement report lists arfcn,pci,rsrp,rsrq per cell.
	for _, m := range nrMeasRe.FindAllStringSubmatch(raw, -1) {
		f := strings.Split(strings.TrimPrefix(m[2], ","), ",")
		for i := 0; i+3 < len(f); i += 4 {
			a, ok1 := atoiDash(f[i])
			p, ok2 := atoiDash(f[i+1])
			if !ok1 || !ok2 {
				continue
			}
			c := radioCell{RAT: "NR", Role: "neighbour", Arfcn: a, PCI: p, RSRP: f[i+2], RSRQ: f[i+3]}
			if b := nrBandFromArfcn(f[i]); b > 0 {
				c.Band = "n" + strconv.Itoa(b)
			}
			c.Key = cellKey("NR", a, p)
			c.Serving = serving(c.RAT, c.Arfcn, c.PCI)
			if !seen[c.Key] {
				seen[c.Key] = true
				neigh = append(neigh, c)
			}
		}
	}
	if neigh == nil {
		neigh = []radioCell{}
	}
	res["neighbours"] = neigh
	// An NSA 5G leg has no signal values in QCAINFO; the NR measurement
	// report has them for the same cell.
	for i, c := range carriers {
		if c.RSRP != "" {
			continue
		}
		for _, n := range neigh {
			if n.Key == c.Key {
				carriers[i].RSRP, carriers[i].RSRQ = n.RSRP, n.RSRQ
			}
		}
	}
	return res
}

// setCellLock replaces the modem's cell locks. An empty lte list / nil nr
// clears that lock. persist keeps the locks across a reboot.
//
// Clearing a lock that isn't set makes the modem answer +CME ERROR: 904
// (seen on "Unlock all" with only an LTE lock, or none), so a clear is only
// sent for a lock that's there, and a 904 on a clear still counts as done.
func setCellLock(lte []lteCell, nr *nrCell, persist bool) (map[string]any, error) {
	if len(lte) > 10 {
		return nil, routerErrf("The modem takes at most 10 LTE cells")
	}
	cur, err := atQuery([]string{`AT+QNWLOCK="common/4g"`, `AT+QNWLOCK="common/5g"`}, 600*time.Millisecond)
	if err != nil {
		return nil, err
	}
	state := parseCellLock(cur)
	lteLocked := len(state["lte"].([]lteCell)) > 0
	nrLocked := state["nr"] != nil

	type step struct {
		cmd   string
		clear bool
	}
	var steps []step
	if len(lte) == 0 {
		if lteLocked {
			steps = append(steps, step{`AT+QNWLOCK="common/4g",0`, true})
		}
	} else {
		parts := []string{fmt.Sprintf(`AT+QNWLOCK="common/4g",%d`, len(lte))}
		for _, c := range lte {
			if c.Earfcn <= 0 || c.Earfcn > 262143 || c.PCI < 0 || c.PCI > 503 {
				return nil, routerErrf("LTE cell needs an EARFCN and a PCI (0-503)")
			}
			parts = append(parts, strconv.Itoa(c.Earfcn), strconv.Itoa(c.PCI))
		}
		steps = append(steps, step{strings.Join(parts, ","), false})
	}
	if nr == nil {
		if nrLocked {
			steps = append(steps, step{`AT+QNWLOCK="common/5g",0`, true})
		}
	} else {
		validSCS := map[int]bool{15: true, 30: true, 60: true, 120: true, 240: true}
		if nr.PCI < 0 || nr.PCI > 1007 || nr.Arfcn <= 0 || nr.Arfcn > 3279165 || nr.Band <= 0 || !validSCS[nr.SCS] {
			return nil, routerErrf("5G cell needs a PCI (0-1007), an ARFCN, the SCS (15/30/…) and a band")
		}
		steps = append(steps, step{fmt.Sprintf(`AT+QNWLOCK="common/5g",%d,%d,%d,%d`, nr.PCI, nr.Arfcn, nr.SCS, nr.Band), false})
	}
	save := 0
	if persist {
		save = 1
	}
	steps = append(steps, step{fmt.Sprintf(`AT+QNWLOCK="save_ctrl",%d,%d`, save, save), false})

	for _, s := range steps {
		raw, err := atQuery([]string{s.cmd}, 1500*time.Millisecond)
		if err != nil {
			return nil, err
		}
		if strings.Contains(raw, "ERROR") && !(s.clear && strings.Contains(raw, "904")) {
			return nil, routerErrf("Modem rejected %s: %s", strings.TrimPrefix(s.cmd, "AT+"), atErrorLine(raw))
		}
	}
	invalidate("cellular")
	return getCellLock()
}

// atErrorLine picks the ERROR line out of an AT reply.
func atErrorLine(raw string) string {
	for _, l := range strings.Split(raw, "\n") {
		if l = strings.TrimSpace(l); strings.Contains(l, "ERROR") {
			return l
		}
	}
	return strings.TrimSpace(raw)
}

// ------------------------------------------------------------ cell names ---
// Nicknames for cells ("Tower behind the church"), kept by the panel next to
// .env, keyed lte:<earfcn>:<pci> / nr:<arfcn>:<pci>.

var cellNamesMu sync.Mutex

func cellNamesPath() string { return filepath.Join(filepath.Dir(envFilePath), "cell_names.json") }

func cellNames() map[string]string {
	cellNamesMu.Lock()
	defer cellNamesMu.Unlock()
	m := map[string]string{}
	if b, err := os.ReadFile(cellNamesPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

var cellKeyRe = regexp.MustCompile(`^(lte|nr):\d{1,7}:\d{1,4}$`)

func setCellName(key, name string) (map[string]string, error) {
	if !cellKeyRe.MatchString(key) {
		return nil, routerErrf("Bad cell key %q", key)
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 40 {
		return nil, routerErrf("Keep the name under 40 characters")
	}
	m := cellNames()
	cellNamesMu.Lock()
	defer cellNamesMu.Unlock()
	if name == "" {
		delete(m, key)
	} else {
		m[key] = name
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(cellNamesPath(), b, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// ------------------------------------------------------- SIM own number ---
// The SIM's own number lives in EF_MSISDN (6F40). Most operators write it,
// some (seen with Yettel RS) leave it empty, so neither the stock UI nor
// AT+CNUM has one. The RG520N has no "ON" phonebook (AT+CPBS only lists
// SM/DC/MC/ME/RC/EN), so the record is written straight through AT+CRSM -
// it's a normal PIN1-protected file, the same thing a phone's "My number"
// setting writes.

const efMSISDN = 28480 // 0x6F40

var (
	crsmRe      = regexp.MustCompile(`\+CRSM:\s*(\d+),(\d+)(?:,"([0-9A-Fa-f]*)")?`)
	msisdnRe    = regexp.MustCompile(`^\+?\d{3,20}$`)
	msisdnSep   = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
	fcpRecLenRe = regexp.MustCompile(`^62[0-9A-F]{2}8205[0-9A-F]{4}([0-9A-F]{4})`)
)

// encodeMSISDN builds one EF_MSISDN record: alpha tag padded with FF, then
// the BCD length, TON/NPI, 10 bytes of swapped-nibble BCD, CCP and EXT.
func encodeMSISDN(num string, recLen int, alpha []byte) (string, error) {
	if recLen < 14 {
		return "", fmt.Errorf("record too short (%d)", recLen)
	}
	ton := byte(0x81)
	if strings.HasPrefix(num, "+") {
		ton, num = 0x91, num[1:]
	}
	if len(num) > 20 {
		return "", fmt.Errorf("number longer than 20 digits")
	}
	rec := make([]byte, recLen)
	for i := range rec {
		rec[i] = 0xFF
	}
	x := recLen - 14
	copy(rec[:x], alpha)
	digits := num
	if len(digits)%2 == 1 {
		digits += "F"
	}
	rec[x] = byte(1 + len(digits)/2)
	rec[x+1] = ton
	for i := 0; i < len(digits); i += 2 {
		lo, _ := strconv.ParseUint(digits[i:i+1], 16, 8)
		hi, _ := strconv.ParseUint(digits[i+1:i+2], 16, 8)
		rec[x+2+i/2] = byte(hi<<4 | lo)
	}
	return strings.ToUpper(hex.EncodeToString(rec)), nil
}

// setSIMNumber writes num as the SIM's own number (record 1 of EF_MSISDN),
// keeping the record's alpha tag ("My number" etc.) if it already has one.
func setSIMNumber(num string) (map[string]string, error) {
	num = msisdnSep.Replace(strings.TrimSpace(num))
	if !msisdnRe.MatchString(num) {
		return nil, routerErrf("Enter the number in international format, like +381601234567")
	}
	raw, err := atQuery([]string{
		fmt.Sprintf(`AT+CRSM=192,%d`, efMSISDN),
		fmt.Sprintf(`AT+CRSM=178,%d,1,4,0`, efMSISDN),
	}, 900*time.Millisecond)
	if err != nil {
		return nil, err
	}
	replies := crsmRe.FindAllStringSubmatch(raw, -1)
	// GET RESPONSE: a UICC answers with an FCP template (62 .. 82 05 <desc>
	// <coding> <record length> <records>); an old 2G SIM doesn't, and then the
	// record we read back gives the length instead.
	if len(replies) < 1 || replies[0][1] != "144" {
		return nil, routerErrf("This SIM has no own-number file (EF_MSISDN): %s", strings.TrimSpace(raw))
	}
	recLen := 0
	if m := fcpRecLenRe.FindStringSubmatch(strings.ToUpper(replies[0][3])); m != nil {
		v, _ := strconv.ParseUint(m[1], 16, 16)
		recLen = int(v)
	}
	var alpha []byte
	if len(replies) > 1 && replies[1][1] == "144" {
		if cur, err := hex.DecodeString(replies[1][3]); err == nil {
			if recLen == 0 {
				recLen = len(cur)
			}
			if len(cur) == recLen && recLen > 14 {
				alpha = cur[:recLen-14]
			}
		}
	}
	if recLen == 0 {
		return nil, routerErrf("Couldn't read the SIM's own-number record size")
	}
	rec, err := encodeMSISDN(num, recLen, alpha)
	if err != nil {
		return nil, routerErrf("%v", err)
	}
	raw, err = atQuery([]string{
		fmt.Sprintf(`AT+CRSM=220,%d,1,4,%d,"%s"`, efMSISDN, recLen, rec),
		`AT+CNUM`,
	}, 1200*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if m := crsmRe.FindStringSubmatch(raw); m == nil || m[1] != "144" {
		sw := strings.TrimSpace(raw)
		if m != nil && m[1] == "105" && m[2] == "130" {
			sw = "the SIM only lets the operator change it (needs PIN2/ADM)"
		}
		return nil, routerErrf("SIM refused the number: %s", sw)
	}
	invalidate("simnumber", "cellular")
	got := ""
	if m := cnumRe.FindStringSubmatch(raw); m != nil {
		got = m[1]
	}
	return map[string]string{"number": got}, nil
}
