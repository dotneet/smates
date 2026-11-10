// smates' reader of the JDK Flight Recorder files of a recorded Gradle test run (see
// internal/runner/gradle_record.go). Run in source-file mode: java Collect.java <dir>.
//
// For every *.jfr file of <dir> it prints, tab-separated, one line per distinct fact:
//   jvm <file name>        starts the facts of one recording
//   command <value>        sun.java.command (names the Gradle test executor)
//   class <name>           a class the JVM loaded
//   read <path>            a file the JVM read
//   proc <pid> <dir> <command>  a process the JVM started
//   error <message>        the recording could not be read (wholly or in part)
// Backslashes, tabs and line breaks inside values are escaped as \\, \t, \n and \r.
import java.io.BufferedWriter;
import java.io.OutputStreamWriter;
import java.nio.charset.StandardCharsets;
import java.nio.file.DirectoryStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import jdk.jfr.consumer.RecordedClass;
import jdk.jfr.consumer.RecordedEvent;
import jdk.jfr.consumer.RecordingFile;

public class Collect {
    public static void main(String[] args) throws Exception {
        List<Path> files = new ArrayList<>();
        try (DirectoryStream<Path> dir = Files.newDirectoryStream(Paths.get(args[0]), "*.jfr")) {
            for (Path p : dir) {
                files.add(p);
            }
        }
        Collections.sort(files);
        BufferedWriter out = new BufferedWriter(new OutputStreamWriter(System.out, StandardCharsets.UTF_8));
        for (Path p : files) {
            out.write("jvm\t" + esc(p.getFileName().toString()) + "\n");
            Set<String> seen = new HashSet<>();
            try (RecordingFile rf = new RecordingFile(p)) {
                while (rf.hasMoreEvents()) {
                    String line = line(rf.readEvent());
                    if (line != null && seen.add(line)) {
                        out.write(line);
                        out.write('\n');
                    }
                }
            } catch (Exception e) {
                out.write("error\t" + esc(String.valueOf(e)) + "\n");
            }
        }
        out.flush();
    }

    private static String line(RecordedEvent e) {
        switch (e.getEventType().getName()) {
            case "jdk.ClassLoad": {
                RecordedClass c = e.getValue("loadedClass");
                return c == null ? null : "class\t" + esc(c.getName());
            }
            case "jdk.FileRead": {
                String path = e.getString("path");
                return path == null ? null : "read\t" + esc(path);
            }
            case "jdk.ProcessStart":
                return "proc\t" + e.getLong("pid") + "\t" + esc(e.getString("directory")) + "\t" + esc(e.getString("command"));
            case "jdk.InitialSystemProperty":
                return "sun.java.command".equals(e.getString("key")) ? "command\t" + esc(e.getString("value")) : null;
            default:
                return null;
        }
    }

    private static String esc(String s) {
        if (s == null) {
            return "";
        }
        StringBuilder b = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '\\': b.append("\\\\"); break;
                case '\t': b.append("\\t"); break;
                case '\n': b.append("\\n"); break;
                case '\r': b.append("\\r"); break;
                default: b.append(c);
            }
        }
        return b.toString();
    }
}
