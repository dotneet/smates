package com.example.app;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

class CatalogTest {
    @Test
    void items() throws Exception {
        assertEquals(2, new Catalog().items().size());
    }
}
