package com.example.pricing;

import java.io.IOException;
import java.io.InputStream;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;

/** Amounts in cents; the currency symbol comes from the rates resource. */
public final class Money {
    private Money() {}

    public static long cents(long units) {
        return units * 100;
    }

    public static String symbol() {
        try (InputStream in = Money.class.getResourceAsStream("/rates.txt")) {
            return new String(in.readAllBytes(), StandardCharsets.UTF_8).split("=")[0].trim();
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }
}
