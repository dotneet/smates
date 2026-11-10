package com.example.store;

import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.util.Properties;

/** Reads config/store.properties relative to the working directory. */
public class Settings {
    public static Properties load() throws IOException {
        Properties p = new Properties();
        try (InputStream in = new FileInputStream("config/store.properties")) {
            p.load(in);
        }
        return p;
    }
}
