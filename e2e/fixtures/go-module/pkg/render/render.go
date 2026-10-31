// Package render formats customer-facing text from embedded templates.
package render

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"money": Money,
}).ParseFS(files, "templates/*.tmpl"))

// Money formats cents as "12.34".
func Money(cents int) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// InvoiceData is the input of the invoice template.
type InvoiceData struct {
	Customer string
	Subtotal int
	Discount int
	Tax      int
	Total    int
	Currency string
}

// Greeting renders the greeting line for a customer.
func Greeting(name string) (string, error) {
	return exec("greeting.tmpl", map[string]string{"Name": name})
}

// Invoice renders a plain-text invoice.
func Invoice(d InvoiceData) (string, error) {
	return exec("invoice.tmpl", d)
}

func exec(name string, data any) (string, error) {
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}
