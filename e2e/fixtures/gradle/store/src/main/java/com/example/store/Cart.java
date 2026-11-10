package com.example.store;

import com.example.pricing.Money;

public class Cart {
    private long units;

    public void add(long n) {
        units += n;
    }

    public long totalCents() {
        return Money.cents(units);
    }

    public String label() {
        return Money.symbol() + " " + totalCents();
    }
}
