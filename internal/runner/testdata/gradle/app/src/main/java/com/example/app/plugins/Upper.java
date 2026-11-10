package com.example.app.plugins;

import java.util.function.UnaryOperator;

public class Upper implements UnaryOperator<String> {
    @Override
    public String apply(String s) {
        return s.toUpperCase();
    }
}
