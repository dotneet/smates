# Proposal: JVM tests (Gradle + JUnit Platform)

Status: **accepted**, in progress: phases 1 to 3 (the Java static analyzer, the
`gradle` runner, recording) are implemented; see "Phases". The decisions taken on the
questions the proposal left open are at the end. As each phase lands, the parts that
describe behavior move into
`DESIGN.md` (section 2) and the user guides; this file is not part of the documentation
site.

## Goals and non-goals

Goals:

- Select and run Java test classes of Gradle builds that run on the JUnit Platform
  (Jupiter, and other engines Gradle reports per class), locally and on CI.
- The same contract as the other runners (DESIGN.md section 1): a test is skipped only
  when it last passed and every input it was observed or computed to depend on hashes
  the same; every uncertainty resolves to running more.
- Nothing to change in the user's build: smates injects what it needs for one
  invocation, as it does for the other runners.

Non-goals for the first version:

- Maven (sketched under "Later"), Kotlin, Groovy and Scala sources (the static
  analyzer reads Java only; see "Static analysis"), Android builds.
- Test-method granularity. The unit is the test class's source file.
- Skipping compilation. Gradle still configures the build, resolves dependencies and
  compiles every source set the test task needs. smates can only save the time spent
  *executing* tests, and the documentation must say so up front: in a build whose test
  task is short compared with configuration and compilation, the gain is small, even if
  selection is precise.

## 1. Suites and units

```yaml
suites:
  - name: app
    runner: gradle
    root: app                    # the Gradle project directory
    # command: ./gradlew :app:test   default: the wrapper found from root upward, and the
    #                                project's `test` task
```

- **Detection** (`smates init`): a `settings.gradle` / `settings.gradle.kts` and, for
  each project directory it includes (and the root project), a `src/test/java`
  directory. One suite per project. The default command runs the nearest `gradlew`
  between the suite root and the repository root (else `gradle`) with the project's
  task path (`:app:test`, or `:test` for the root project). A suite can name another
  `Test` task in `command` (for example a task that filters JUnit tags).
- **Test files**: `src/test/java/**/*.java` relative to the suite root by default
  (`tests:` overrides it as for any suite).
- **Units**: a test file is a unit when it declares a top-level, non-abstract class that
  may run tests: it carries a JUnit annotation (`@Test`, `@ParameterizedTest`,
  `@RepeatedTest`, `@TestFactory`, `@TestTemplate`, `@Nested`), or extends, through the
  static index, a class that does. Other files under the test source roots are test
  support code: dependencies of the units that use them, not units themselves (like
  Go's non-test files of a test package or pytest's helper modules).
- **Unit ID**: the source file. Its class name comes from its `package` declaration and
  file name. `@Nested` classes belong to the file of their outer class: Gradle's
  `--tests com.example.FooTest` filter runs `FooTest$Inner` too (verified with the
  prototype).
- **Passing units**: the init script (section 2) reads the selected class names from a
  file and adds one `filter.includeTestsMatching(<class name>)` per unit, so a long
  selection never reaches an argument length limit. It also sets
  `filter.failOnNoMatchingTests = false`: a unit whose class was deleted or that the
  task's own filters exclude must not fail the build.
- **A full run** (`All`) adds no filter, so Gradle's own test detection runs everything
  the task would run.

## 2. Injection: an init script

smates passes `--init-script <shims>/smates.init.gradle` and configures it through
`SMATES_*` environment variables. The script is Groovy, interpreted by Gradle itself: no
plugin, no jar, no compilation step, shipped as text like the other shims. It touches
only the `Test` tasks named in `SMATES_GRADLE_TASKS` and:

- adds the unit filters (section 1);
- when recording, or when smates asks for fresh outcomes: sets
  `outputs.upToDateWhen { false }` and `outputs.cacheIf { false }`. Gradle's test task is
  incremental and cacheable: with the same inputs and filters it would be `UP-TO-DATE`
  or `FROM-CACHE`, run nothing, and leave nothing to trace;
- when recording: sets `forkEvery = 1` (section 3.1) and adds the JFR option to
  `jvmArgs` (section 3.2);
- copies the tracing variables into the task's `environment`, so that Node, Bun and
  Python children of Java tests trace themselves (the test JVM's environment is the
  task's, not necessarily the client's);
- registers a `TestListener` (runs in the Gradle daemon) that writes one line per test
  and per class: class, test, result, assumption failure, and the executor it ran in
  (`Gradle Test Executor N`, the descriptor's parent);
- writes the task's facts smates needs to map paths: its `workingDir`, the source and
  output directories of the source sets on its classpath, and the Java executable of its
  `javaLauncher`.

**Configuration cache.** The prototype stored and reused a configuration cache entry
with such a script, and the listener wrote its events on reuse. Environment variables
the script reads are inputs of the entry, so every value that changes per run (a fresh
temporary trace directory) invalidates it, and Gradle configures the build again. The
trace directory therefore has a stable path per suite (`.smates/tmp/gradle/<suite>`),
emptied before each run.

## 3. Recording

### 3.1 One JVM per test class

A class loads once per JVM. Attributing code to a test class through class loading
therefore needs a JVM per test class while recording: the init script sets
`forkEvery = 1`, and Gradle still runs `maxParallelForks` of them at once.

- Cost: one JVM start and JUnit Platform start per class (about 0.3 to 0.5 s on a laptop
  in the prototype), spread over the parallel forks. Selective runs start only the
  selected classes; full runs on the default branch pay it for every class. This is the
  same trade-off as `granularity: func` for Go and `isolate: true` for jest.
- Behavior changes: classes no longer share a JVM. State one class leaves behind for the
  next is gone, and fixtures started once per JVM (an embedded database, a container
  started from a static initializer) start once per class while recording.
- `isolate: false` keeps Gradle's forking. Every class then depends on everything its
  JVM loaded and read, for all the classes that JVM ran: still sound (dependencies are
  a superset), less precise, faster to record.
- Gradle also forks a JVM for candidate classes that run no test (an abstract base
  class, a nested class run through its outer class). Such a JVM has no test events
  and is discarded.

### 3.2 JDK Flight Recorder instead of an agent

Each test JVM gets
`-XX:StartFlightRecording=filename=<trace>/jvm-%p.jfr,dumponexit=true,settings=<shims>/smates.jfc`.
The settings file enables only:

| Event | Settings | Gives |
|---|---|---|
| `jdk.ClassLoad` | threshold 0, no stack trace | the classes the JVM loaded: code dependencies |
| `jdk.FileRead` | threshold 0, no stack trace | files read through `FileInputStream`, `RandomAccessFile` and `FileChannel` (`Files.readString`, `Files.newInputStream`), and classpath resources loaded from directories |
| `jdk.ProcessStart` | no stack trace (JDK 17+) | the command line and directory of every child process |
| `jdk.InitialSystemProperty` | at chunk start | `sun.java.command`, which names the Gradle executor of the JVM |

Why not a Java agent: the agent would need a jar, built with a JDK, either shipped as a
binary or compiled on the user's machine. An agent dumping its own JFR recording from a
shutdown hook also proved unreliable in the prototype: JFR's own shutdown hook stopped
the recording first, and the dump failed with missing chunks. `dumponexit` leaves the
dump to JFR.

Why not JaCoCo: it instruments bytecode and adds another jar (EPL licensed) to ship.
Class loading already gives file-level code dependencies, the granularity smates
records: a loaded class is a superset of an executed one.

### 3.3 Reading the recordings

JFR files are binary. After the run, smates starts one JVM with a collector written as a
single Java source file and launched in source-file mode (`java Collect.java <dir>`,
JDK 11+). It reads every recording with `jdk.jfr.consumer.RecordingFile` and prints a
compact line-oriented summary per JVM (executor, classes, reads, processes). Shipped as
text, compiled in memory, no jar. It runs on the Java executable of the test task (the
init script writes its path), so it can always read the recordings that JVM wrote. In
the prototype it read eight recordings in about 2.5 s, compilation included.

A JFR reader in Go would remove that JVM start, at the cost of implementing the chunk,
metadata and constant pool format. It can replace the collector later without changing
anything else.

### 3.4 Attribution

The listener's events say which classes ran in which executor (`Gradle Test Executor N`).
The recording says which executor the JVM was (`sun.java.command` ends with the quoted
executor name). Joining the two gives, per JVM, the test classes it ran and therefore the
units it belongs to. Executor numbers keep increasing for the lifetime of a daemon, so
they are matched within one build only.

- What the JVM read and loaded before its first test (Gradle's worker, JUnit's
  discovery) belongs to that JVM's class. Gradle's own jars and the JDK lie outside the
  repository and drop out.
- Reads of `.class` files under the build output are left out: class loads stand for
  them (next section).
- Lambda and hidden classes (`com.example.Store$$Lambda+0x...`) map to their enclosing
  class.

### 3.5 Mapping to repository files

- **Classes to sources.** `com.example.Foo$Bar` maps to the top-level class
  `com.example.Foo`, then through the static index (section 5) to every Java source
  declaring a top-level type of that name. A class not in the index (a dependency, a
  generated source) adds nothing.
- **Generated sources** (annotation processors, code generated from schemas) are not in
  the repository. The source files that drive their generation must be declared with
  `rules` or `inputs`.
- **Resources.** A read below a source set's output resources directory
  (`build/resources/main/x.sql`) maps to the file in the source set's resource
  directories (`src/main/resources/x.sql`). The init script exports the actual
  directories, so non-default layouts map too.
- **Resources inside a jar** of one of the repository's own projects (project
  dependencies on the runtime classpath can be jars). JFR shows the read of the jar,
  not of the entry, so the unit depends on that project's whole resource tree
  (`src/main/resources/**`). This is coarse but safe. Class loads from such a jar still
  map by name.
- **Relative paths** resolve against the task's `workingDir`. Paths outside the
  repository are dropped, and git-ignored ones are hashed from disk, as for every
  runner.
- **Directory listings** (`Files.list`, `File.listFiles`, a migration tool scanning a
  classpath directory) and **existence checks** (`Files.exists`) are not JFR events.
  Rule: a unit that read a file also depends on the listing (`dir/`) of the directory
  holding it. Adding a sibling (a new migration script next to the ones the test read)
  then selects the test. This adds dependencies and never removes them. A test that
  only lists a directory, or only probes paths, is a known gap, covered by `rules`.
- **Child processes** go through the existing command-line analysis (DESIGN.md section
  2, "Subprocesses smates cannot trace"). A JVM before 17 records no `ProcessStart`, and
  its children are not seen at all (a `doctor` warning).

### 3.6 Statuses

Statuses come from the listener's events, per test and then per unit:

| Outcome of the unit's tests | Status |
|---|---|
| a test failed | `fail` |
| a test was aborted by an assumption (Gradle's `TestResult.getAssumptionFailure()`; in JUnit XML a `<skipped>` whose type is `org.opentest4j.TestAbortedException`) | `unknown` |
| tests passed, the rest skipped without an assumption (`@Disabled`) | `pass` |
| no event for the unit (the task filters it out, or it runs no test) | `unknown`, with a warning |
| the build failed before tests ran (a compile error) | `fail`, `Inferred` |

An assumption usually checks the environment ("a database is reachable"). Counting the
unit as passed would let a run without the database mark it passed. A later run where
the database is present would then skip a test that never really ran. The prototype
confirmed that Gradle tells assumptions apart from `@Disabled` both in the listener and
in the XML report.

Without recording the same listener runs. It is cheap, and unlike the XML reports it
cannot return results that an up-to-date task left behind from an earlier build.

## 4. Environment variables

JFR has no event for `System.getenv`. Java units therefore record no `$NAME`
dependencies, and changing an environment variable selects none of them (the suite's
`env:` is still part of its settings). This is a documented limitation. Hooking
`System.getenv` would need an agent retransforming a JDK class, which is out of scope.
System properties set in the build script are covered because the build files are suite
inputs (section 6).

## 5. Static analysis (Java sources, no toolchain)

A new `internal/static/java` implements `host.Analyzer`. Like the other analyzers, it
reads the working tree, not build output: class files reflect the last build, not
`HEAD`, and the static graph must reflect the tree as it is now.

**Index.** For every `.java` file below a source root: its package, and the names of its
top-level types (`class`, `interface`, `enum`, `record`, `@interface` at brace depth 0).
Java allows several top-level types per file, and only the public one has to match the
file name.

**Direct dependencies of a file**, after a tokenizer that skips comments, strings,
text blocks and character literals:

1. single-type imports (`import a.b.C;`, `import a.b.C.D;`): the file declaring the
   longest prefix that is a top-level type; static imports (`import static a.b.C.m;`,
   `a.b.C.*`): the file of `C`;
2. on-demand imports (`import a.b.*;`): each identifier of the file that names a
   top-level type of package `a.b`;
3. the file's own package: each identifier that names a top-level type of the same
   package, in any source root (`main` and `test` share packages, so do several
   projects). No import is needed there, so identifiers are matched by name;
4. qualified names in code (`a.b.C.m()`): the longest prefix in the index.

Matching identifiers by name over-approximates (a local variable named like a class
adds a dependency). That costs runs, never misses. FQCN collisions across projects
resolve to every candidate.

**What only the static graph sees:**

- compile-time constants, which `javac` inlines into the classes that use them (the
  prototype's test of a `static final String` never loaded the class declaring it);
- annotations and types used only in signatures;
- references added since the last recording.

**What only recording sees:** classes reached through reflection, dependency injection
or `ServiceLoader` (the prototype's `Class.forName` target), and resources and data
files. A file outside the static closure that a unit loaded is recorded as usual.

## 6. Suite inputs

These files can change the outcome of every unit of a suite, so they are suite inputs:
`settings.gradle(.kts)`, the `build.gradle(.kts)` of the project and of its parents,
`gradle.properties`, version catalogs (`gradle/*.versions.toml`),
`gradle/wrapper/gradle-wrapper.properties`, dependency lockfiles (`gradle.lockfile`),
`buildSrc/**` and included builds (`includeBuild`) as `dir/**`.

## 7. `init` and `doctor`

- `smates init` writes a `gradle` suite per project with Java tests. A build it detects
  but cannot support (Maven, Kotlin-only sources) is reported, with a suggested
  `ignore:` glob for its tree. Without that, every change to it would be an orphan and
  run the other suites in full.
- `smates doctor` checks the test JVM (JFR needs 11+; child processes need 17+) and the
  Gradle version (assumption failures in the listener; minimum to be pinned down).

## 8. Verification

- A fixture `e2e/fixtures/gradle` with what static analysis misses and what Java makes
  special:
  - a class reached only by reflection;
  - an inlined constant;
  - a package-private secondary top-level class;
  - nested tests;
  - an abstract base class with inherited tests;
  - a classpath resource, and relative file reads through `FileInputStream` and NIO;
  - a directory a test scans for new files;
  - a child process;
  - an assumption-aborted test and a disabled test;
  - a second project used as a project dependency;
  - a task that filters JUnit tags.
- Scenarios and mutations as for the other fixtures (DESIGN.md section 7). CI needs a
  JDK and a cached Gradle distribution.

### Prototype

A throwaway prototype (JDK 21, Gradle 9.4, JUnit Jupiter 5.11) checked the assumptions
above:

| Question | Result |
|---|---|
| Are classes reached only by reflection seen? | yes, `jdk.ClassLoad` |
| Is a class whose `static final` constant a test uses seen? | no: inlined, needs the static graph |
| Are reads through `FileInputStream`, `Files.readString` and `getResourceAsStream` seen? | yes, `jdk.FileRead` (relative paths stay relative) |
| Is a directory listing seen? | no |
| Is a child process seen? | yes, its command line (`jdk.ProcessStart`) |
| Can a JVM be matched to its test classes? | yes, through `sun.java.command` and the listener's executor names |
| Does an agent's shutdown-hook dump work? | no, JFR's own hook stops the recording first |
| Does `--tests Outer` run `@Nested` classes? | yes |
| Can assumption failures be told apart from `@Disabled`? | yes, in the listener and in the XML report |
| Does the init script survive the configuration cache? | yes: stored, reused, listener events written |

## 9. Phases

1. Java static analyzer and index (section 5), with unit tests. This is useful on its
   own as the safety net. **Done** (`internal/static/java`): indexing and resolving a
   tree of 5,200 files (40 MB) takes about 0.3 s. No runner follows `.java` files yet,
   so selection is unchanged until phase 2 adds `gradle` to the runners whose static
   closure counts (`staticLangs` in `internal/selector`).
2. The `gradle` runner without recording: suites, units, the init script's filters and
   listener, statuses (section 3.6), suite inputs. **Done** (`internal/runner/gradle.go`,
   `shims/gradle/smates.init.gradle`; DESIGN.md section 2, "Gradle"). Until phase 3, a pass
   is recorded as `unknown` (nothing traced what the class used), so a recorded unit runs
   every time; units without an entry are decided by their static closure against the
   base revision. Differences from the sections above: the init script reads its
   variables when a `Test` task starts, not while the build is configured, and applies to
   every `Test` task of the build (no `SMATES_GRADLE_TASKS`); classes go through the
   `--tests` patterns, which Gradle intersects with the task's own filters, rather than
   `filter.includeTestsMatching`, which it would add to them; every `build.gradle(.kts)`
   of the build is a suite input, not only the project's and its parents'. User
   documentation, `init` and `doctor` stay for phase 4.
3. Recording: `forkEvery`, JFR, the collector, attribution and path mapping. **Done**
   (`internal/runner/gradle_record.go`, `shims/gradle/smates.jfc`, `Collect.java`;
   DESIGN.md section 2, "Recording Gradle tests"). Differences from the sections above:
   recording needs Java 17, not 11 (`%p` in the recording's file name and
   `jdk.ProcessStart`); the facts cover the jar and resource and class output
   directories of every project of the build, through a provider stored in the
   configuration cache; a process started without a directory is analyzed in the task's
   working directory (JFR records none); a self-tracing Node, Bun or Python child is
   matched to its JVM by the pids of `ProcessStart`, else given to every unit.
4. `init`, `doctor`, user documentation, the e2e fixture.
5. Later: Maven (`-Dtest=`, `forkCount=1`/`reuseForks=false`, `argLine` for JFR, the
   Surefire XML reports), Kotlin sources (top-level functions and properties, `FooKt`
   classes).

## Decisions

1. **JFR reader**: the Java collector in source-file mode (section 3.3). A Go reader may
   replace it later.
2. **Isolation**: one JVM per test class while recording by default; `isolate: false`
   keeps Gradle's forking and merges dependencies per JVM (section 3.1).
3. **Conditional skips** (`@EnabledOnOs`, `@EnabledIfEnvironmentVariable`, ...) count
   like `@Disabled`: they do not keep a unit from passing. A static scan adding `$NAME`
   for `@EnabledIfEnvironmentVariable(named = "NAME")` may come later.
4. **Units a tag-filtered task never runs** are `unknown`, with a warning. Evaluating
   class-level `@Tag` statically may come later.
5. **Minimum versions**: JDK 17 to record (phase 3: below it the units run unrecorded,
   their passes `unknown`). Gradle's minimum is pinned down with phase 4 (`doctor`): a
   Gradle without `TestResult.getAssumptionFailure` runs, but every skip counts as an
   assumption.
