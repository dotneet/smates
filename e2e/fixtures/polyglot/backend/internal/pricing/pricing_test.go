package pricing

import (
	"testing"

	"example.com/polyglot/backend/internal/money"
)

func TestTotal(t *testing.T) {
	cases := []struct {
		line Line
		want money.Cents
	}{
		{Line{"ABC-001", 1, 999}, 999},
		{Line{"ABC-001", 10, 100}, 950},
		{Line{"ABC-001", 100, 100}, 8500},
	}
	for _, c := range cases {
		if got := Total(c.line); got != c.want {
			t.Errorf("Total(%+v) = %d, want %d", c.line, got, c.want)
		}
	}
}
