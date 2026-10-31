package store

import "testing"

func TestLoadUsers(t *testing.T) {
	us, err := LoadUsers("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	gold := 0
	for _, u := range us {
		if u.Tier == "gold" {
			gold++
		}
	}
	if len(us) != 3 || gold != 2 {
		t.Fatalf("users = %+v", us)
	}
}

func TestLoadOrders(t *testing.T) {
	os, err := LoadOrders("testdata/orders.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(os) != 2 || os[0].SKU != "A1" {
		t.Fatalf("orders = %+v", os)
	}
}
