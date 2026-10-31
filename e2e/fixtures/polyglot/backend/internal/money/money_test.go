package money

import "testing"

func TestFormat(t *testing.T) {
	cases := map[Cents]string{0: "$0.00", 5: "$0.05", 1234: "$12.34", -250: "-$2.50"}
	for in, want := range cases {
		if got := Format(in); got != want {
			t.Errorf("Format(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPercent(t *testing.T) {
	if got := Percent(1999, 10); got != 200 {
		t.Errorf("Percent(1999, 10) = %d, want 200", got)
	}
}
