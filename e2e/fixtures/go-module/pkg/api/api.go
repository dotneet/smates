// Package api implements checkout on top of calc and render.
package api

import (
	"errors"

	"example.com/shop/pkg/calc"
	"example.com/shop/pkg/render"
)

// TaxPermille is the tax rate applied at checkout.
const TaxPermille = 100

// Order is a checkout request.
type Order struct {
	Customer    string
	Lines       []calc.Line
	DiscountPct int
}

// Quote is the priced order.
type Quote struct {
	Subtotal, Discount, Tax, Total int
}

// Price computes a quote.
func Price(o Order) (Quote, error) {
	if len(o.Lines) == 0 {
		return Quote{}, errors.New("empty order")
	}
	sub := calc.Subtotal(o.Lines)
	after := calc.ApplyDiscount(sub, o.DiscountPct)
	tax := calc.Tax(after, TaxPermille)
	return Quote{Subtotal: sub, Discount: sub - after, Tax: tax, Total: after + tax}, nil
}

// Checkout prices an order and renders the invoice.
func Checkout(o Order) (string, error) {
	q, err := Price(o)
	if err != nil {
		return "", err
	}
	return render.Invoice(render.InvoiceData{
		Customer: o.Customer, Subtotal: q.Subtotal, Discount: q.Discount,
		Tax: q.Tax, Total: q.Total, Currency: "USD",
	})
}
