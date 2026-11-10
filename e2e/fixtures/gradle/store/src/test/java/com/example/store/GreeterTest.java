package com.example.store;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.example.store.support.Names;
import org.junit.jupiter.api.Test;

class GreeterTest {
    @Test
    void greets() {
        // Names.ANY is a compile-time constant: javac inlines it, so the test never loads
        // Names; only the static graph links them.
        assertEquals("hello ada", new Greeter().greet(Names.ANY));
    }
}
