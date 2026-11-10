package com.example.store;

import static org.junit.jupiter.api.Assertions.assertTrue;

import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.List;
import java.util.stream.Collectors;
import java.util.stream.Stream;

import org.junit.jupiter.api.Test;

class MigrationsTest {
    @Test
    void everyMigrationCreatesATable() throws Exception {
        List<Path> files;
        try (Stream<Path> s = Files.list(Paths.get("data/migrations"))) {
            files = s.sorted().collect(Collectors.toList());
        }
        assertTrue(files.size() >= 2);
        for (Path f : files) {
            assertTrue(Files.readString(f).startsWith("create table"), f.toString());
        }
    }
}
