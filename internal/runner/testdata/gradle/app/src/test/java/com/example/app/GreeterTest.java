package com.example.app;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.example.app.support.Names;
import org.junit.jupiter.api.Test;

class GreeterTest {
    @Test
    void greets() {
        assertEquals("hello " + Names.ANY, new Greeter().greet(Names.ANY));
    }
}
