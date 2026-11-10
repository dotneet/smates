package com.example.store;

import com.example.store.support.BaseCheck;

class GreeterCheck extends BaseCheck {
    @Override
    protected boolean ok() {
        return new Greeter().greet("x").startsWith("hello");
    }
}
