package com.example.store;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;

/** The store test task excludes the tag: this class never runs there. */
@Tag("slow")
class SlowTest {
    @Test
    void slow() {
        assertEquals(4, 2 + 2);
    }
}
