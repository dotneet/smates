package region

import "testing"

func TestName(t *testing.T) {
	if Name() == "" {
		t.Log("empty region")
	}
}
