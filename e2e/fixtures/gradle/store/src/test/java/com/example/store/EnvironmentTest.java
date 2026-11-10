package com.example.store;

import static org.junit.jupiter.api.Assumptions.assumeTrue;

import org.junit.jupiter.api.Test;

/** Aborted by an assumption without the service: never proven to pass. */
class EnvironmentTest {
    @Test
    void needsService() {
        assumeTrue(System.getenv("BOOKSHOP_SERVICE_URL") != null, "no service");
    }
}
