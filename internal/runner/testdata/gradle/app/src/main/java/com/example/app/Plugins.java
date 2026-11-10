package com.example.app;

import java.util.function.UnaryOperator;

public class Plugins {
    @SuppressWarnings("unchecked")
    public static UnaryOperator<String> load(String name) throws ReflectiveOperationException {
        return (UnaryOperator<String>) Class.forName("com.example.app.plugins." + name).getDeclaredConstructor().newInstance();
    }
}
