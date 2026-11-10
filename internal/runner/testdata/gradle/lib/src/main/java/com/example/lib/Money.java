package com.example.lib;

public final class Money {
    private Money() {}

    public static long cents(long units) {
        return units * 100;
    }
}
