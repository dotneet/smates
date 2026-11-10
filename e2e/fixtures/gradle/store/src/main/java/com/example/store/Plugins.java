package com.example.store;

import java.util.function.UnaryOperator;

public class Plugins {
    @SuppressWarnings("unchecked")
    public static UnaryOperator<String> load(String name) throws ReflectiveOperationException {
        return (UnaryOperator<String>) Class.forName("com.example.store.plugins." + name).getDeclaredConstructor().newInstance();
    }
}
