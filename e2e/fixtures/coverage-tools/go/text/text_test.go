package text

import "testing"

func TestGreeting(t *testing.T) {
	if g := Greeting(); g != "hello" {
		t.Fatalf("Greeting() = %q", g)
	}
}
