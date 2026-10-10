package main

import (
	"reflect"
	"testing"
)

func TestUCS2RoundTrip(t *testing.T) {
	for _, s := range []string{"Telekom Service", "Müller", "Серёжа", "日本"} {
		if got := ucs2Decode(ucs2Hex(s)); got != s {
			t.Errorf("%q -> %q", s, got)
		}
	}
	if ucs2Hex("Ab") != "00410062" {
		t.Errorf("ucs2Hex = %s", ucs2Hex("Ab"))
	}
	// a GSM-charset reply passes through untouched
	if ucs2Decode("Notruf") != "Notruf" {
		t.Error("GSM name mangled")
	}
}

func TestParseContacts(t *testing.T) {
	raw := `+CPBS: "SM",3,250

OK
+CPBR: 16,"112",129,"004E006F0074007200750066"
+CPBR: 10,"498003302202",145,"00540065006C0065006B006F006D"
+CPBR: 3,"+49170","145","00410062"
+CPBR: 5,"2000",129,"004D00FC006C006C00650072"

OK`
	got := parseContacts(raw)
	want := []contact{{5, "Müller", "2000"}, {16, "Notruf", "112"}, {10, "Telekom", "+498003302202"}}
	if !reflect.DeepEqual(got["contacts"], want) {
		t.Errorf("contacts = %v", got["contacts"])
	}
	if got["used"] != 3 || got["total"] != 250 {
		t.Errorf("used/total = %v/%v", got["used"], got["total"])
	}
}

func TestVCF(t *testing.T) {
	in := "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Doe;John;;;\r\nFN:John Doe\r\nTEL;TYPE=CELL:+1 (555) 010-2000\r\nTEL;TYPE=HOME:555\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\nVERSION:2.1\nN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:M=C3=BCller;Anna\nTEL;CELL:0170123\nEND:VCARD\n" +
		"BEGIN:VCARD\nVERSION:4.0\nFN:Long name that keeps\n  going\nTEL;VALUE=uri:tel:+4917012\nEND:VCARD\n" +
		"BEGIN:VCARD\nFN:No number\nEND:VCARD\n"
	got := parseVCF(in)
	want := []contact{{0, "John Doe", "+15550102000"}, {0, "Anna Müller", "0170123"}, {0, "Long name that keeps going", "+4917012"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseVCF = %+v", got)
	}
	out := contactsVCF([]contact{{1, "A, B; C", "+49"}})
	if out != "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:A\\, B\\; C\r\nN:A\\, B\\; C;;;;\r\nTEL;TYPE=CELL:+49\r\nEND:VCARD\r\n" {
		t.Errorf("contactsVCF = %q", out)
	}
	back := parseVCF(out)
	if len(back) != 1 || back[0].Name != "A, B; C" {
		t.Errorf("round trip = %+v", back)
	}
}
