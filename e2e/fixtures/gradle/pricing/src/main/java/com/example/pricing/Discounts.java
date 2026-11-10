package com.example.pricing;

public final class Discounts {
    private Discounts() {}

    public static long apply(long cents, String code) {
        return cents - Coupon.off(code, cents);
    }
}

// A package-private second class of the file: its class file is Coupon.class, its source
// Discounts.java.
class Coupon {
    static long off(String code, long cents) {
        return "HALF".equals(code) ? cents / 2 : 0;
    }
}
