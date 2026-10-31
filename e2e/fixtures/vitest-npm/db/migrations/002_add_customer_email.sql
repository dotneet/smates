ALTER TABLE customers ADD COLUMN email TEXT;
CREATE UNIQUE INDEX customers_email ON customers (email);
