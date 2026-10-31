package settings

import "testing"

func TestLoadCI(t *testing.T) {
	s, err := Load("testdata", "ci")
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Port: 8080, Currency: "USD", MaxQuantity: 500}
	if s != want {
		t.Fatalf("settings = %+v, want %+v", s, want)
	}
}
