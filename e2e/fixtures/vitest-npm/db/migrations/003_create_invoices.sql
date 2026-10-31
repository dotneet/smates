CREATE TABLE invoices (
  id TEXT PRIMARY KEY,
  customer_id INTEGER NOT NULL REFERENCES customers (id),
  total_cents INTEGER NOT NULL
);
