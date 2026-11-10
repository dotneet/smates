package com.example.store;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

class PluginsTest {
    @Test
    void loadsByName() throws Exception {
        assertEquals("AB", Plugins.load("Upper").apply("ab"));
    }
}
