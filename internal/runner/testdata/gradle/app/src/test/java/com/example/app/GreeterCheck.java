package com.example.app;

import com.example.app.support.BaseCheck;

class GreeterCheck extends BaseCheck {
    @Override
    protected boolean ok() {
        return new Greeter().greet("x").startsWith("hello");
    }
}
