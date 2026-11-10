package com.example.store;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.util.List;
import org.junit.jupiter.api.Test;

class CatalogTest {
    @Test
    void items() throws Exception {
        assertEquals(List.of("apple", "pear"), new Catalog().items());
    }
}
