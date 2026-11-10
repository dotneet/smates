package com.example.store.support;

import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.Test;

/** Inherited tests: the classes extending it are the test classes. */
public abstract class BaseCheck {
    protected abstract boolean ok();

    @Test
    void holds() {
        assertTrue(ok());
    }
}
