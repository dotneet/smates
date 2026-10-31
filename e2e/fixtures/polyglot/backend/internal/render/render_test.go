package render

import "testing"

func TestReceipt(t *testing.T) {
	got, err := Receipt("Ada", []Item{{"Apple", 1140}, {"Pear", 760}})
	if err != nil {
		t.Fatal(err)
	}
	want := "Receipt for Ada\n- Apple: $11.40\n- Pear: $7.60\n\n"
	if got != want {
		t.Fatalf("receipt =\n%q\nwant\n%q", got, want)
	}
}
