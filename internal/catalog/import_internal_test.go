package catalog

import "testing"

func TestParseRupiah(t *testing.T) {
	ok := map[string]int64{
		"25000": 25000, "25.000": 25000, "25,000": 25000, "Rp 25.000": 25000, "Rp25000": 25000, "1.250.000": 1250000,
		"25000.00": 25000, "25000,0": 25000, "0": 0, " 15000 ": 15000,
	}
	for in, want := range ok {
		if got, valid := parseRupiah(in); !valid || int64(got) != want {
			t.Errorf("parseRupiah(%q) = %d, %v; want %d", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "18.5", "18,50", "2.50", "-100", "25.00.0", "1000000001", "abc", "25 000x", "99999999999999999999"} {
		if got, valid := parseRupiah(in); valid {
			t.Errorf("parseRupiah(%q) = %d, want refused", in, got)
		}
	}
}
