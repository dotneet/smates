package hints

import "testing"

func TestServiceLink(t *testing.T) {
	in := baseInput(map[string]string{"fixtures/prices.json": "j2"})
	in.Service = func(f string) bool { return f == "fixtures/prices.json" }
	h := Compute(in)
	if len(h.Candidates) != 1 || h.Candidates[0].Link != LinkService {
		t.Errorf("candidates %+v", h.Candidates)
	}
}
