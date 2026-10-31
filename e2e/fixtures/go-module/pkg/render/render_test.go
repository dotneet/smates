package render

import "testing"

func TestGreeting(t *testing.T) {
	got, err := Greeting("Ada")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello, Ada!" {
		t.Fatalf("Greeting = %q", got)
	}
}

func TestMoney(t *testing.T) {
	if got := Money(1205); got != "12.05" {
		t.Fatalf("Money = %q", got)
	}
}
