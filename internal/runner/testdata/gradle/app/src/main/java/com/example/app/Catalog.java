package com.example.app;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.stream.Collectors;

public class Catalog {
    public List<String> items() throws IOException {
        try (InputStream in = Catalog.class.getResourceAsStream("/catalog.txt");
                BufferedReader r = new BufferedReader(new InputStreamReader(in, StandardCharsets.UTF_8))) {
            return r.lines().collect(Collectors.toList());
        }
    }
}
