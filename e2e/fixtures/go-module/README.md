# shop

A tiny order-pricing service used as an end-to-end fixture for smates.

- `pkg/calc`: pure money arithmetic (cents)
- `pkg/render`: text templates embedded with `//go:embed`
- `pkg/api`: checkout = calc + render
- `pkg/limits`: business limits, kept in sync with `config/settings.yaml`
- `internal/store`: JSON-backed records (fixtures in `internal/store/testdata`)
- `cmd/tool`: command line quote tool (`go run ./cmd/tool -sku A1 -qty 3`)
