package main

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------------ cell lock ---
// AT+QNWLOCK pins the modem to specific cells: LTE to up to 10 EARFCN/PCI
// pairs, 5G to one PCI/ARFCN/SCS/band. "save_ctrl" decides whether a lock
// survives a reboot (kept in the modem's NV, so nothing on the router side has
// to re-apply it). Syntax confirmed with AT+QNWLOCK=? on the RG520N.

var (
	qnwlock4gRe   = regexp.MustCompile(`\+QNWLOCK:\s*"common/4g",(\d+)((?:,\d+,\d+)*)`)
	qnwlock5gRe   = regexp.MustCompile(`\+QNWLOCK:\s*"common/5g",(\d+)(?:,(\d+),(\d+),(\d+))?`)
	qnwlockSaveRe = regexp.MustCompile(`\+QNWLOCK:\s*"save_ctrl",(\d),(\d)`)
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

// nrSCS maps the QENG <scs> code to kHz, the unit QNWLOCK takes.
var nrSCS = map[string]int{"0": 15, "1": 30, "2": 60, "3": 120, "4": 240}

func getCellLock() (map[string]any, error) {
	raw, err := atQuery([]string{
		`AT+QNWLOCK="common/4g"`,
		`AT+QNWLOCK="common/5g"`,
		`AT+QNWLOCK="save_ctrl"`,
		`AT+QENG="servingcell"`,
	}, 600*time.Millisecond)
	if err != nil {
		return nil, err
	}
	return parseCellLock(raw), nil
}

// parseCellLock reads the lock state and the serving cell(s) out of one AT
// reply block, so the page can offer "lock to the cell I'm on now".
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

	// The cell(s) the modem is on right now, in the fields QNWLOCK wants.
	if f := lteQENGFields(raw); len(f) >= 7 {
		e, err1 := strconv.Atoi(f[5])
		p, err2 := strconv.Atoi(f[4])
		if err1 == nil && err2 == nil {
			res["serving_lte"] = lteCell{e, p}
		}
	}
	var nr []string // pci, arfcn, band, scs code
	if f := qengFields(raw, `"NR5G-NSA",`); len(f) >= 10 {
		nr = []string{f[2], f[6], f[7], f[9]}
	} else if f := qengFields(raw, `"NR5G-SA",`); len(f) >= 10 {
		nr = []string{f[2], f[6], f[7], f[9]}
	} else if f := qengFields(raw, `"servingcell",`); len(f) >= 15 && f[1] == "NR5G-SA" {
		nr = []string{f[6], f[8], f[9], f[14]}
	}
	if nr != nil {
		pci, err1 := strconv.Atoi(nr[0])
		arfcn, err2 := strconv.Atoi(nr[1])
		band, _ := strconv.Atoi(nr[2])
		if band == 0 {
			band = nrBandFromArfcn(nr[1]) // v1 firmware reports band 0
		}
		if scs, ok := nrSCS[nr[3]]; ok && err1 == nil && err2 == nil && band > 0 {
			res["serving_nr"] = nrCell{pci, arfcn, scs, band}
		}
	}
	return res
}

// setCellLock replaces the modem's cell locks. An empty lte list / nil nr
// clears that lock. persist keeps the locks across a reboot.
func setCellLock(lte []lteCell, nr *nrCell, persist bool) (map[string]any, error) {
	if len(lte) > 10 {
		return nil, routerErrf("The modem takes at most 10 LTE cells")
	}
	cmds := []string{}
	if len(lte) == 0 {
		cmds = append(cmds, `AT+QNWLOCK="common/4g",0`)
	} else {
		parts := []string{fmt.Sprintf(`AT+QNWLOCK="common/4g",%d`, len(lte))}
		for _, c := range lte {
			if c.Earfcn <= 0 || c.Earfcn > 262143 || c.PCI < 0 || c.PCI > 503 {
				return nil, routerErrf("LTE cell needs an EARFCN and a PCI (0-503)")
			}
			parts = append(parts, strconv.Itoa(c.Earfcn), strconv.Itoa(c.PCI))
		}
		cmds = append(cmds, strings.Join(parts, ","))
	}
	if nr == nil {
		cmds = append(cmds, `AT+QNWLOCK="common/5g",0`)
	} else {
		validSCS := map[int]bool{15: true, 30: true, 60: true, 120: true, 240: true}
		if nr.PCI < 0 || nr.PCI > 1007 || nr.Arfcn <= 0 || nr.Arfcn > 3279165 || nr.Band <= 0 || !validSCS[nr.SCS] {
			return nil, routerErrf("5G cell needs a PCI (0-1007), an ARFCN, the SCS (15/30/…) and a band")
		}
		cmds = append(cmds, fmt.Sprintf(`AT+QNWLOCK="common/5g",%d,%d,%d,%d`, nr.PCI, nr.Arfcn, nr.SCS, nr.Band))
	}
	save := 0
	if persist {
		save = 1
	}
	cmds = append(cmds, fmt.Sprintf(`AT+QNWLOCK="save_ctrl",%d,%d`, save, save))
	raw, err := atQuery(cmds, 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if strings.Contains(raw, "ERROR") {
		return nil, routerErrf("Modem rejected the cell lock: %s", strings.TrimSpace(raw))
	}
	return getCellLock()
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
