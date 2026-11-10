package com.example.lib;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

class MoneyTest {
    @Test
    void cents() {
        assertEquals(300, Money.cents(3));
    }
}
