// Package store loads JSON records.
package store

import (
	"encoding/json"
	"fmt"
	"os"
)

// User is a customer record.
type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Tier string `json:"tier"`
}

// Order is a stored order.
type Order struct {
	ID     int    `json:"id"`
	UserID int    `json:"user_id"`
	SKU    string `json:"sku"`
	Qty    int    `json:"qty"`
}

// LoadUsers reads a JSON array of users.
func LoadUsers(path string) ([]User, error) {
	var us []User
	if err := load(path, &us); err != nil {
		return nil, err
	}
	for _, u := range us {
		if u.ID <= 0 || u.Name == "" {
			return nil, fmt.Errorf("%s: invalid user %+v", path, u)
		}
	}
	return us, nil
}

// LoadOrders reads a JSON array of orders.
func LoadOrders(path string) ([]Order, error) {
	var os []Order
	if err := load(path, &os); err != nil {
		return nil, err
	}
	return os, nil
}

// Validate checks that a file is a JSON array of objects.
func Validate(path string) error {
	var v []map[string]any
	return load(path, &v)
}

func load(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
