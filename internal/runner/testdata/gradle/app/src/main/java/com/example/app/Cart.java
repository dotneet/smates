package com.example.app;

import com.example.lib.Money;

public class Cart {
    private long units;

    public void add(long n) {
        units += n;
    }

    public long totalCents() {
        return Money.cents(units);
    }
}
