# bookshop

A small Gradle build (two projects, JUnit Jupiter) used to exercise smates' `gradle`
runner. `pricing` is a library; `store` depends on it.

    gradle test

The `store` test task excludes the JUnit tag `slow`.
