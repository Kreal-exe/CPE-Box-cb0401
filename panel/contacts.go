package main

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ------------------------------------------------------------- contacts ---
// The SIM's phonebook (the "SM" storage, 250 entries on a Telekom SIM) through
// AT+CPBR / AT+CPBW, like a phone's "SIM contacts". Names go in as UCS2 so
// anything beyond ASCII (umlauts, Cyrillic) round-trips; the modem switches
// to UCS2 for the call and back to GSM afterwards so the stock SMS daemon,
// which expects GSM, keeps working. Numbers are up to 40 digits, names up to
// 16 UCS2 characters (AT+CPBW=? on the RG520N).

const (
	pbNameMax = 16
	pbNumMax  = 40
)

type contact struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Number string `json:"number"`
}

var (
	cpbrRe    = regexp.MustCompile(`\+CPBR:\s*(\d+),"([^"]*)",(\d+),"([^"]*)"`)
	cpbsRe    = regexp.MustCompile(`\+CPBS:\s*"SM",(\d+),(\d+)`)
	cpbwErrRe = regexp.MustCompile(`\+CME ERROR:\s*(\d+)`)
	pbNumRe   = regexp.MustCompile(`^\+?[0-9*#]{1,40}$`)
)

// pbCommands wraps cmds so the phonebook is the SIM and names are UCS2, and
// restores the GSM charset afterwards whatever happened in between.
func pbCommands(cmds ...string) []string {
	return append(append([]string{`AT+CPBS="SM"`, `AT+CSCS="UCS2"`}, cmds...), `AT+CSCS="GSM"`)
}

// ucs2Hex encodes s as the hex of its UTF-16BE code units.
func ucs2Hex(s string) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u>>8), byte(u))
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// ucs2Decode turns a UCS2 hex string back into text; anything that isn't
// valid hex is returned as it came (a GSM-charset reply).
func ucs2Decode(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil || len(b)%2 != 0 {
		return s
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00￿")
}

// getContacts reads the whole SIM phonebook. 250 entries take the modem
// under a second; the reply is one line per used slot.
func getContacts() (map[string]any, error) {
	raw, err := atQuery(pbCommands(`AT+CPBS?`, `AT+CPBR=?`, fmt.Sprintf(`AT+CPBR=1,%d`, 250)), 2500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	return parseContacts(raw), nil
}

func parseContacts(raw string) map[string]any {
	var list []contact
	for _, m := range cpbrRe.FindAllStringSubmatch(raw, -1) {
		idx, _ := strconv.Atoi(m[1])
		num := m[2]
		if m[3] == "145" && !strings.HasPrefix(num, "+") {
			num = "+" + num
		}
		list = append(list, contact{idx, ucs2Decode(m[4]), num})
	}
	if list == nil {
		list = []contact{}
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	res := map[string]any{"contacts": list, "used": len(list), "total": 0}
	if m := cpbsRe.FindStringSubmatch(raw); m != nil {
		res["used"], _ = strconv.Atoi(m[1])
		res["total"], _ = strconv.Atoi(m[2])
	}
	return res
}

func cleanNumber(num string) string {
	return strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(num))
}

// saveContact writes one entry: index 0 takes the first free slot, otherwise
// the slot is overwritten (edit).
func saveContact(index int, name, number string) (contact, error) {
	name = strings.TrimSpace(name)
	number = cleanNumber(number)
	if !pbNumRe.MatchString(number) {
		return contact{}, routerErrf("Enter a phone number, like +49 170 1234567")
	}
	if name == "" {
		return contact{}, routerErrf("The contact needs a name")
	}
	if n := len([]rune(name)); n > pbNameMax {
		return contact{}, routerErrf("SIM names are at most %d characters (this one is %d)", pbNameMax, n)
	}
	if index < 0 || index > 250 {
		return contact{}, routerErrf("Bad SIM slot %d", index)
	}
	typ := 129
	if strings.HasPrefix(number, "+") {
		typ = 145
	}
	slot := ""
	if index > 0 {
		slot = strconv.Itoa(index)
	}
	cmd := fmt.Sprintf(`AT+CPBW=%s,"%s",%d,"%s"`, slot, number, typ, ucs2Hex(name))
	raw, err := atQuery(pbCommands(cmd, `AT+CPBS?`), 2500*time.Millisecond)
	if err != nil {
		return contact{}, err
	}
	if m := cpbwErrRe.FindStringSubmatch(raw); m != nil || !strings.Contains(raw, "OK") {
		return contact{}, routerErrf("The SIM refused the contact: %s", pbErrorText(m))
	}
	invalidate("contacts")
	if index == 0 {
		// find the slot the modem picked, so the page can select it
		if list, err := getContacts(); err == nil {
			for _, c := range list["contacts"].([]contact) {
				if c.Number == number && c.Name == name {
					return c, nil
				}
			}
		}
	}
	return contact{index, name, number}, nil
}

func deleteContact(index int) error {
	if index <= 0 || index > 250 {
		return routerErrf("Bad SIM slot %d", index)
	}
	raw, err := atQuery(pbCommands(fmt.Sprintf(`AT+CPBW=%d`, index)), 2000*time.Millisecond)
	if err != nil {
		return err
	}
	if m := cpbwErrRe.FindStringSubmatch(raw); m != nil {
		return routerErrf("The SIM refused to delete: %s", pbErrorText(m))
	}
	invalidate("contacts")
	return nil
}

// deleteContacts clears several slots at once, 10 per AT batch so the port
// isn't held for long. Returns how many the SIM refused.
func deleteContacts(indexes []int) (int, error) {
	var cmds []string
	for _, i := range indexes {
		if i <= 0 || i > 250 {
			return 0, routerErrf("Bad SIM slot %d", i)
		}
		cmds = append(cmds, fmt.Sprintf(`AT+CPBW=%d`, i))
	}
	failed := 0
	for i := 0; i < len(cmds); i += 10 {
		end := i + 10
		if end > len(cmds) {
			end = len(cmds)
		}
		raw, err := atQuery(pbCommands(cmds[i:end]...), 1200*time.Millisecond)
		if err != nil {
			return failed, err
		}
		failed += len(cpbwErrRe.FindAllString(raw, -1))
	}
	invalidate("contacts")
	return failed, nil
}

func pbErrorText(m []string) string {
	if m == nil {
		return "no reply"
	}
	switch m[1] {
	case "20":
		return "the SIM phonebook is full"
	case "21":
		return "that slot doesn't exist"
	case "24":
		return "the name is too long for this SIM"
	case "26":
		return "the number is too long for this SIM"
	case "17", "18":
		return "the SIM wants its PIN2"
	}
	return "CME error " + m[1]
}

// ------------------------------------------------------------------ vCard ---

// contactsVCF writes the phonebook as vCard 3.0, one card per entry - what
// phones, Google and Apple Contacts import.
func contactsVCF(list []contact) string {
	var b strings.Builder
	for _, c := range list {
		fmt.Fprintf(&b, "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:%s\r\nN:%s;;;;\r\nTEL;TYPE=CELL:%s\r\nEND:VCARD\r\n",
			vcfEscape(c.Name), vcfEscape(c.Name), c.Number)
	}
	return b.String()
}

func vcfEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

var vcfQPRe = regexp.MustCompile(`=([0-9A-Fa-f]{2})`)

// parseVCF pulls name + first phone number out of each card in a .vcf file,
// tolerant of vCard 2.1/3.0/4.0, folded lines, TYPE params and
// quoted-printable names (old Nokia/Android exports).
func parseVCF(data string) []contact {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\n ", "") // unfold
	data = strings.ReplaceAll(data, "\n\t", "")
	var out []contact
	var cur *contact
	for _, line := range strings.Split(data, "\n") {
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "BEGIN:VCARD"):
			cur = &contact{}
		case strings.HasPrefix(up, "END:VCARD"):
			if cur != nil && cur.Number != "" {
				if cur.Name == "" {
					cur.Name = cur.Number
				}
				out = append(out, *cur)
			}
			cur = nil
		case cur == nil:
		default:
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			params := strings.ToUpper(key)
			name := params
			if i := strings.IndexAny(params, ";"); i >= 0 {
				name = params[:i]
			}
			if strings.Contains(params, "QUOTED-PRINTABLE") {
				val = vcfQPRe.ReplaceAllStringFunc(val, func(h string) string {
					b, _ := hex.DecodeString(h[1:])
					return string(b)
				})
			}
			switch name {
			case "FN":
				cur.Name = vcfUnescape(val)
			case "N":
				if cur.Name == "" {
					p := strings.Split(val, ";")
					parts := []string{}
					if len(p) > 1 && p[1] != "" {
						parts = append(parts, p[1])
					}
					if p[0] != "" {
						parts = append(parts, p[0])
					}
					cur.Name = vcfUnescape(strings.Join(parts, " "))
				}
			case "TEL":
				if cur.Number == "" {
					v := strings.TrimPrefix(val, "tel:")
					if n := cleanNumber(v); pbNumRe.MatchString(n) {
						cur.Number = n
					}
				}
			}
		}
	}
	return out
}

func vcfUnescape(s string) string {
	return strings.TrimSpace(strings.NewReplacer(`\n`, " ", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(s))
}

// importContacts writes list to the SIM's free slots, skipping entries that
// are already there (same number). Names longer than the SIM takes are cut.
// One AT batch per 10 contacts keeps each port hold short.
func importContacts(list []contact) (map[string]any, error) {
	cur, err := getContacts()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, c := range cur["contacts"].([]contact) {
		have[c.Number] = true
	}
	used, total := cur["used"].(int), cur["total"].(int)
	var cmds []string
	added, skipped := 0, 0
	for _, c := range list {
		num := cleanNumber(c.Number)
		if have[num] || !pbNumRe.MatchString(num) {
			skipped++
			continue
		}
		if total > 0 && used+added >= total {
			break
		}
		name := []rune(strings.TrimSpace(c.Name))
		if len(name) > pbNameMax {
			name = name[:pbNameMax]
		}
		typ := 129
		if strings.HasPrefix(num, "+") {
			typ = 145
		}
		cmds = append(cmds, fmt.Sprintf(`AT+CPBW=,"%s",%d,"%s"`, num, typ, ucs2Hex(string(name))))
		have[num] = true
		added++
	}
	failed := 0
	for i := 0; i < len(cmds); i += 10 {
		end := i + 10
		if end > len(cmds) {
			end = len(cmds)
		}
		raw, err := atQuery(pbCommands(cmds[i:end]...), 1500*time.Millisecond)
		if err != nil {
			return nil, err
		}
		failed += len(cpbwErrRe.FindAllString(raw, -1))
	}
	invalidate("contacts")
	return map[string]any{"added": added - failed, "skipped": skipped, "failed": failed, "left": len(list) - added - skipped}, nil
}
