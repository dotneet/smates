package com.example.app;

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
    class Empty {
        @Test
        void zero() {
            assertEquals(0, new Cart().totalCents());
        }
    }
}
