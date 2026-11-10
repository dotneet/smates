package com.example.app;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.nio.charset.StandardCharsets;

import org.junit.jupiter.api.Test;

class ShellTest {
    @Test
    void motd() throws Exception {
        Process p = new ProcessBuilder("sh", "-c", "cat data/motd.txt").start();
        String out = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        assertEquals(0, p.waitFor());
        assertEquals("welcome\n", out);
    }
}
