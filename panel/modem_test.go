package main

import (
	"encoding/hex"
	"reflect"
	"testing"
)

// Captured live from an RG520N on Telekom.de (5G NSA, B3 anchor + n1).
const capturedLockReply = `AT+QNWLOCK="common/4g"
+QNWLOCK: "common/4g",0

OK
AT+QNWLOCK="common/5g"
+QNWLOCK: "common/5g",0

OK
AT+QNWLOCK="save_ctrl"
+QNWLOCK: "save_ctrl",0,0

OK
AT+QENG="servingcell"
+QENG: "servingcell","NOCONN"
+QENG: "LTE","FDD",262,01,1929500,321,1300,3,5,5,34BA,-75,-8,-50,24,12,120,-
+QENG: "NR5G-NSA",262,01,774,-82,30,-11,431070,1,3,0

OK
`

func TestParseCellLockUnlocked(t *testing.T) {
	got := parseCellLock(capturedLockReply)
	if l := got["lte"].([]lteCell); len(l) != 0 {
		t.Errorf("lte = %v, want none", l)
	}
	if got["nr"] != nil || got["persist"] != false {
		t.Errorf("nr/persist = %v/%v, want nil/false", got["nr"], got["persist"])
	}
	if s := got["serving_lte"]; s != (lteCell{1300, 321}) {
		t.Errorf("serving_lte = %v", s)
	}
	if s := got["serving_nr"]; s != (nrCell{PCI: 774, Arfcn: 431070, SCS: 15, Band: 1}) {
		t.Errorf("serving_nr = %v", s)
	}
}

func TestParseCellLockLocked(t *testing.T) {
	raw := `+QNWLOCK: "common/4g",2,1300,321,6300,12
+QNWLOCK: "common/5g",774,431070,15,1
+QNWLOCK: "save_ctrl",1,1
`
	got := parseCellLock(raw)
	if l := got["lte"]; !reflect.DeepEqual(l, []lteCell{{1300, 321}, {6300, 12}}) {
		t.Errorf("lte = %v", l)
	}
	if n := got["nr"]; n != (nrCell{774, 431070, 15, 1}) {
		t.Errorf("nr = %v", n)
	}
	if got["persist"] != true {
		t.Errorf("persist = %v", got["persist"])
	}
}

func TestParseCellLockSA(t *testing.T) {
	raw := `+QENG: "servingcell","NOCONN","NR5G-SA","TDD",262,01,1A2B3C,512,ABCD,643296,78,12,-90,-11,18,1,-
`
	if s := parseCellLock(raw)["serving_nr"]; s != (nrCell{512, 643296, 30, 78}) {
		t.Errorf("serving_nr = %v", s)
	}
}

func TestEncodeMSISDN(t *testing.T) {
	// The record as read back from a real Telekom SIM (AT+CRSM=178,28480,1,4,0).
	const real = "456967656E65205275666E756D6D65720891947186249484F6FFFFFFFFFF"
	alpha, _ := hex.DecodeString(real[:32])
	got, err := encodeMSISDN("+4917684249486", 30, alpha)
	if err != nil || got != real {
		t.Errorf("encode = %s, %v\nwant     %s", got, err, real)
	}
	// Even digit count, national number, no alpha tag.
	got, _ = encodeMSISDN("0601234567", 14, nil)
	if want := "06816010325476FFFFFFFFFFFFFF"; got != want {
		t.Errorf("encode = %s, want %s", got, want)
	}
}

func TestFcpRecordLength(t *testing.T) {
	m := fcpRecLenRe.FindStringSubmatch("621E82054221001E0483026F40A5038001618A01058B036F060F800200788800")
	if m == nil || m[1] != "001E" {
		t.Errorf("record length = %v, want 001E", m)
	}
}

func TestDiagHost(t *testing.T) {
	for _, h := range []string{"8.8.8.8", "one.one.one.one", "2606:4700:4700::1111"} {
		if !diagHostRe.MatchString(h) {
			t.Errorf("%q rejected", h)
		}
	}
	for _, h := range []string{"-c 99 x", "a;reboot", "$(id)", "a b", ""} {
		if diagHostRe.MatchString(h) {
			t.Errorf("%q accepted", h)
		}
	}
}
