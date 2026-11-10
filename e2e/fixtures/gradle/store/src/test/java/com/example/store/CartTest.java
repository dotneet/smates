package com.example.store;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Disabled;
import org.junit.jupiter.api.Nested;
import org.junit.jupiter.api.Test;

class CartTest {
    @Test
    void total() {
        Cart c = new Cart();
        c.add(2);
        assertEquals(200, c.totalCents());
    }

    @Test
    @Disabled("not yet")
    void discount() {}

    @Nested
    class Labels {
        @Test
        void label() {
            Cart c = new Cart();
            c.add(1);
            assertEquals("EUR 100", c.label());
        }
    }
}
