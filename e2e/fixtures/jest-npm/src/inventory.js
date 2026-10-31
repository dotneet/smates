'use strict';

class Inventory {
  constructor(stock = {}) {
    this.stock = { ...stock };
  }

  available(sku) {
    return this.stock[sku] || 0;
  }

  reserve(sku, qty) {
    if (this.available(sku) < qty) {
      throw new Error(`insufficient stock for ${sku}`);
    }
    this.stock[sku] -= qty;
    return this.stock[sku];
  }

  release(sku, qty) {
    this.stock[sku] = this.available(sku) + qty;
    return this.stock[sku];
  }

  lowStock(threshold) {
    return Object.keys(this.stock)
      .filter((sku) => this.stock[sku] <= threshold)
      .sort();
  }
}

module.exports = { Inventory };
