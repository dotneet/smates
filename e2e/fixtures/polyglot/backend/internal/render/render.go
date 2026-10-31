// Package render renders customer receipts from embedded templates.
package render

import (
	"embed"
	"strings"
	"text/template"

	"example.com/polyglot/backend/internal/money"
)

//go:embed templates/*.tmpl
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"money": money.Format}).ParseFS(files, "templates/*.tmpl"))

// Item is one receipt line.
type Item struct {
	Name  string
	Total money.Cents
}

// Receipt renders a receipt.
func Receipt(customer string, items []Item) (string, error) {
	var b strings.Builder
	err := tmpl.ExecuteTemplate(&b, "receipt.tmpl", map[string]any{"Customer": customer, "Items": items})
	return b.String(), err
}
