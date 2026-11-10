package com.example.store.plugins;

import java.util.function.UnaryOperator;

public class Upper implements UnaryOperator<String> {
    @Override
    public String apply(String s) {
        return s.toUpperCase();
    }
}
