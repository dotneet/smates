package cli

// Services and environment the tests need: compose files, localhost URLs and
// environment reads in the code, variables listed in .env.example; and doctor's checks.

import (
	"fmt"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"gopkg.in/yaml.v3"
)

// ---- services ----

// serviceHint is a database or cache the tests appear to need.
type serviceHint struct {
	Kind   string // postgres | mysql | redis | mongo
	Name   string // service name (compose) or the kind
	Image  string
	Env    [][2]string
	Source string // where it was found: "docker-compose.yml", or "src/db.ts (postgres://localhost:5432)"
}

var serviceDefaults = map[string]struct {
	image  string
	port   int
	env    [][2]string
	health string
}{
	"postgres": {"postgres:16", 5432, [][2]string{{"POSTGRES_USER", "postgres"}, {"POSTGRES_PASSWORD", "postgres"}, {"POSTGRES_DB", "postgres"}}, `pg_isready -U postgres`},
	"mysql":    {"mysql:8", 3306, [][2]string{{"MYSQL_ROOT_PASSWORD", "root"}, {"MYSQL_DATABASE", "test"}}, `mysqladmin ping -h 127.0.0.1`},
	"redis":    {"redis:7", 6379, nil, `redis-cli ping`},
	"mongo":    {"mongo:7", 27017, nil, `mongosh --quiet --eval 'db.runCommand({ping: 1})'`},
}

var servicePorts = map[int]string{5432: "postgres", 3306: "mysql", 6379: "redis", 27017: "mongo"}

// imageKind maps a container image to a service kind ("" when not a known one).
func imageKind(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name, _, _ = strings.Cut(name, ":")
	name, _, _ = strings.Cut(name, "@")
	switch {
	case strings.HasPrefix(name, "postgres"), strings.HasPrefix(name, "postgis"), strings.HasPrefix(name, "timescaledb"):
		return "postgres"
	case name == "mysql", name == "mariadb":
		return "mysql"
	case name == "redis", strings.HasPrefix(name, "redis-"), name == "valkey":
		return "redis"
	case name == "mongo", name == "mongodb":
		return "mongo"
	}
	return ""
}

var composeFiles = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

// composeServices returns the known databases and caches defined in the root compose
// file (images and literal environment values; ${VAR} references are dropped).
func composeServices(root string) []serviceHint {
	for _, f := range composeFiles {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]struct {
				Image       string    `yaml:"image"`
				Environment yaml.Node `yaml:"environment"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(data, &doc) != nil {
			return nil
		}
		var out []serviceHint
		for _, name := range slices.Sorted(maps.Keys(doc.Services)) {
			s := doc.Services[name]
			kind := imageKind(s.Image)
			if kind == "" {
				continue
			}
			h := serviceHint{Kind: kind, Name: name, Image: s.Image, Source: f}
			if strings.Contains(h.Image, "$") {
				h.Image = serviceDefaults[kind].image
			}
			h.Env = literalEnv(&s.Environment)
			out = append(out, h)
		}
		return out
	}
	return nil
}

// literalEnv reads a compose environment (mapping or "K=V" list), keeping only
// literal values.
func literalEnv(n *yaml.Node) [][2]string {
	var out [][2]string
	add := func(k, v string) {
		if k != "" && !strings.Contains(v, "$") {
			out = append(out, [2]string{k, v})
		}
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			add(n.Content[i].Value, n.Content[i+1].Value)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if k, v, ok := strings.Cut(c.Value, "="); ok {
				add(k, v)
			}
		}
	}
	return out
}

func envValue(env [][2]string, key, def string) string {
	for _, kv := range env {
		if kv[0] == key {
			return kv[1]
		}
	}
	return def
}

// serviceEnv is the container environment for a service: the compose values, or the
// defaults the image needs to start.
func serviceEnv(h serviceHint) [][2]string {
	if len(h.Env) > 0 {
		env := h.Env
		if h.Kind == "postgres" && envValue(env, "POSTGRES_PASSWORD", "") == "" && envValue(env, "POSTGRES_HOST_AUTH_METHOD", "") == "" {
			env = append(env, [2]string{"POSTGRES_PASSWORD", "postgres"})
		}
		return env
	}
	return serviceDefaults[h.Kind].env
}

// serviceURL is the connection URL tests on the runner use for a service, and the
// variable it conventionally goes in.
func serviceURL(h serviceHint) (string, string) {
	env := serviceEnv(h)
	switch h.Kind {
	case "postgres":
		user := envValue(env, "POSTGRES_USER", "postgres")
		db := envValue(env, "POSTGRES_DB", user)
		return "DATABASE_URL", fmt.Sprintf("postgres://%s:%s@localhost:5432/%s?sslmode=disable", user, envValue(env, "POSTGRES_PASSWORD", ""), db)
	case "mysql":
		user, pass := "root", envValue(env, "MYSQL_ROOT_PASSWORD", envValue(env, "MARIADB_ROOT_PASSWORD", ""))
		if u := envValue(env, "MYSQL_USER", ""); u != "" {
			user, pass = u, envValue(env, "MYSQL_PASSWORD", "")
		}
		return "DATABASE_URL", fmt.Sprintf("mysql://%s:%s@localhost:3306/%s", user, pass, envValue(env, "MYSQL_DATABASE", envValue(env, "MARIADB_DATABASE", "")))
	case "redis":
		return "REDIS_URL", "redis://localhost:6379"
	case "mongo":
		return "MONGO_URL", "mongodb://localhost:27017"
	}
	return "", ""
}

// ---- source scan: localhost URLs and environment reads ----

type localRef struct {
	URL  string // scheme://host:port as written
	Port int
	Kind string // service kind for well-known ports/schemes, "" otherwise
	File string
}

type sourceScan struct {
	Local    []localRef
	EnvReads map[string]string // variable -> first file reading it
}

var (
	localURLRe = regexp.MustCompile(`\b(https?|wss?|postgres(?:ql)?|mysql|redis|mongodb)://(?:[^@/\s'"` + "`" + `]*@)?(localhost|127\.0\.0\.1|0\.0\.0\.0)(?::(\d{2,5}))?`)
	envReadRes = []*regexp.Regexp{
		regexp.MustCompile(`process\.env\.([A-Za-z_][A-Za-z0-9_]*)`),
		regexp.MustCompile(`process\.env\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`),
		regexp.MustCompile(`(?:import\.meta|Bun|Deno)\.env\.([A-Za-z_][A-Za-z0-9_]*)`),
		regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*"([A-Za-z_][A-Za-z0-9_]*)"`),
		regexp.MustCompile(`os\.(?:environ(?:\.get)?|getenv)[\[(]\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`),
	}
	sourceExts = map[string]bool{".go": true, ".ts": true, ".tsx": true, ".mts": true, ".cts": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".py": true}
)

// ownServerRe matches code that starts the server it calls in its own process (an
// httptest server, a listener on port 0, http.server, net.createServer) or intercepts
// its requests (nock, msw, responses, ...): the local URLs of such a file need no server
// started before the tests.
var ownServerRe = regexp.MustCompile(`httptest\.New(?:Unstarted|TLS)?Server|\bnet\.Listen(?:TCP|Unix)?\(|\bListenAndServe(?:TLS)?\(|\.Serve\(|\b(?:Threading)?HTTPServer\(|\b(?:Threading)?TCPServer\(|serve_forever|\.bind\(\(|\bcreateServer\(|\.listen\(|\bsetupServer\(|\bnock\(|\bresponses\.(?:add|get|post|put|delete|patch)\(|requests_mock|\brespx\b|\bhttpretty\b|\baioresponses\b|\bhttpserver\b`)

// localServerPort reports whether a port of a plain local URL (no known service) can
// name a server the tests need: not an invalid port, nor one below 1024, which tests use
// as an address where nothing listens (the discard port 9, ...) rather than for a dev
// server.
func localServerPort(port int) bool { return port >= 1024 && port <= 65535 }

// ownServer reports whether source starts or intercepts the server it calls (ownServerRe).
func ownServer(source []byte) bool { return ownServerRe.Match(source) }

// scanSources looks through the repository's source and test files for URLs pointing
// at a local server and for environment variables the code reads. tested reports
// whether a suite runs a file (nil: every file): a plain local URL counts only there,
// in a file that does not start or intercept that server itself.
func scanSources(root string, files []string, tested func(string) bool) sourceScan {
	sc := sourceScan{EnvReads: map[string]string{}}
	seen := map[string]bool{}
	n := 0
	for _, f := range files {
		if !sourceExts[path.Ext(f)] || strings.Contains("/"+f, "/node_modules/") || strings.Contains("/"+f, "/vendor/") || strings.Contains("/"+f, "/testdata/") {
			continue
		}
		if n++; n > 20000 {
			break
		}
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil || fi.Size() > 1<<20 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		for _, m := range localURLRe.FindAllSubmatch(data, -1) {
			scheme := strings.TrimSuffix(string(m[1]), "ql")
			port, _ := strconv.Atoi(string(m[3]))
			kind := servicePorts[port]
			switch scheme {
			case "postgres", "mysql", "redis":
				kind = scheme
			case "mongodb":
				kind = "mongo"
			}
			if port == 0 && kind != "" {
				port = serviceDefaults[kind].port
			}
			if port == 0 {
				continue // http://localhost without a port: usually a placeholder
			}
			if kind == "" && (!localServerPort(port) || tested != nil && !tested(f) || ownServer(data)) {
				continue
			}
			key := fmt.Sprintf("%s:%d", kind, port)
			if seen[key] {
				continue
			}
			seen[key] = true
			sc.Local = append(sc.Local, localRef{URL: fmt.Sprintf("%s://%s:%d", m[1], m[2], port), Port: port, Kind: kind, File: f})
		}
		for _, re := range envReadRes {
			for _, m := range re.FindAllSubmatch(data, -1) {
				if _, ok := sc.EnvReads[string(m[1])]; !ok {
					sc.EnvReads[string(m[1])] = f
				}
			}
		}
	}
	return sc
}

var envExampleFiles = []string{".env.example", ".env.sample", ".env.template", ".env.dist"}

type envReq struct {
	Name, Example, Source, ReadBy string
}

// envRequirements lists the variables an .env.example (at the root or a suite root)
// declares that the code reads.
func envRequirements(root string, suites []config.Suite, reads map[string]string) []envReq {
	dirs := []string{"."}
	for _, s := range suites {
		if !slices.Contains(dirs, s.Root) {
			dirs = append(dirs, s.Root)
		}
	}
	var out []envReq
	seen := map[string]bool{}
	for _, d := range dirs {
		for _, f := range envExampleFiles {
			rel := joinRel(d, f)
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			for _, l := range strings.Split(string(data), "\n") {
				l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "export "))
				k, v, ok := strings.Cut(l, "=")
				k = strings.TrimSpace(k)
				if !ok || k == "" || strings.HasPrefix(k, "#") || seen[k] {
					continue
				}
				by, read := reads[k]
				if !read {
					continue
				}
				seen[k] = true
				v = strings.TrimSpace(v)
				if i := strings.Index(v, " #"); i >= 0 {
					v = strings.TrimSpace(v[:i])
				}
				out = append(out, envReq{Name: k, Example: strings.Trim(v, `"'`), Source: rel, ReadBy: by})
			}
		}
	}
	return out
}

var secretNameRe = regexp.MustCompile(`(?i)(KEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE)`)

// reachable reports whether something accepts TCP connections on localhost:port.
func reachable(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
