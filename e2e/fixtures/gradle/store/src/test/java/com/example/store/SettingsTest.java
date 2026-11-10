package com.example.store;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

class SettingsTest {
    @Test
    void pageSize() throws Exception {
        assertEquals("20", Settings.load().getProperty("page.size"));
    }
}
