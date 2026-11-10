package com.example.app.support;

import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.Test;

public abstract class BaseCheck {
    protected abstract boolean ok();

    @Test
    void holds() {
        assertTrue(ok());
    }
}
