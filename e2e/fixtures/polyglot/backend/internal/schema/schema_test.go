package schema

import (
	"encoding/json"
	"os"
	"testing"
)

func load(t *testing.T) *Schema {
	t.Helper()
	wd, _ := os.Getwd()
	p, err := Locate(wd)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func order(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestContractVersion(t *testing.T) {
	if s := load(t); s.Title != "Order" || s.Version != 3 {
		t.Fatalf("backend speaks Order v3, shared schema is %s v%d", s.Title, s.Version)
	}
}

func TestValidate(t *testing.T) {
	s := load(t)
	good := order(t, `{"id":"o1","sku":"ABC-123","quantity":2,"unit_cents":499}`)
	if err := s.Validate(good); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	for _, bad := range []string{
		`{"id":"o1","sku":"abc","quantity":2,"unit_cents":499}`,
		`{"id":"o1","sku":"ABC-123","quantity":0,"unit_cents":499}`,
		`{"id":"o1","sku":"ABC-123","unit_cents":499}`,
	} {
		if err := s.Validate(order(t, bad)); err == nil {
			t.Errorf("invalid order accepted: %s", bad)
		}
	}
}
