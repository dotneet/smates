package com.example.pricing;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

class MoneyTest {
    @Test
    void cents() {
        assertEquals(300, Money.cents(3));
    }

    @Test
    void symbol() {
        assertEquals("EUR", Money.symbol());
    }
}
