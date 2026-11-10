package com.example.pricing;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

class CouponTest {
    @Test
    void half() {
        assertEquals(50, Coupon.off("HALF", 100));
    }
}
