package com.example.app;

import static org.junit.jupiter.api.Assumptions.assumeTrue;

import org.junit.jupiter.api.Test;

class EnvironmentTest {
    @Test
    void needsService() {
        assumeTrue(System.getenv("SHOP_SERVICE_URL") != null, "no service");
    }
}
