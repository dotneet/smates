package trace

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// V8 coverage files are mostly function range data. Of a file smates only needs the
// "url" of every script, and to know that the file is complete: a process killed while
// writing leaves a truncated file, whose scripts after the cut would be missing. A
// streaming scanner (v8Scanner) does both in one pass with a bounded buffer, without
// decoding the ranges, and Collect runs it over the files of a trace in parallel.

// errV8Invalid is the error of a coverage file that is not one complete JSON value.
var errV8Invalid = errors.New("truncated coverage file")

// v8URLs extracts script URLs from a V8 coverage JSON file without decoding the
// (large) function range data. It returns errV8Invalid for a file that json.Valid
// would reject (truncated or otherwise malformed): a file cut short anywhere (even
// right after a range object) is incomplete.
func v8URLs(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := v8Bufs.Get().(*[]byte)
	defer v8Bufs.Put(buf)
	return scanV8(f, *buf)
}

// v8Bufs are the read buffers of v8URLs.
var v8Bufs = sync.Pool{New: func() any {
	b := make([]byte, 256<<10)
	return &b
}}

// scanV8 reads one V8 coverage JSON document from r, using buf as its read buffer.
func scanV8(r io.Reader, buf []byte) ([]string, error) {
	s := &v8Scanner{r: r, buf: buf}
	c, ok := s.skipSpace()
	if !ok || !s.value(c, 0) {
		return nil, s.fail()
	}
	if _, more := s.skipSpace(); more || s.err != nil {
		return nil, s.fail()
	}
	return s.urls, nil
}

// maxV8Depth is how deeply arrays and objects may nest, as in encoding/json.
const maxV8Depth = 10000

// v8Scanner validates a JSON document as json.Valid does and collects the URLs the
// byte scan smates used before it would find: the string values of every "url" key,
// at any depth, written with no space around the colon (as JSON.stringify writes
// them), that look like file paths or file:// URLs (see addURL).
type v8Scanner struct {
	r    io.Reader
	buf  []byte
	i, n int   // buf[i:n] is unread
	err  error // a read error (other than io.EOF)
	eof  bool
	key  []byte // raw bytes of the object key being read
	str  []byte // raw bytes of the url value being read
	urls []string
}

// fail returns the error of an invalid document: the read error, if there was one.
func (s *v8Scanner) fail() error {
	if s.err != nil {
		return s.err
	}
	return errV8Invalid
}

// fill reads more input into buf, reporting whether there is any.
func (s *v8Scanner) fill() bool {
	for !s.eof && s.err == nil {
		n, err := s.r.Read(s.buf)
		s.i, s.n = 0, n
		if err == io.EOF {
			s.eof = true
		} else if err != nil {
			s.err = err
		}
		if n > 0 {
			return true
		}
	}
	return false
}

// next returns the next byte. The buffered case is small enough to be inlined.
func (s *v8Scanner) next() (byte, bool) {
	if s.i < s.n {
		c := s.buf[s.i]
		s.i++
		return c, true
	}
	return s.nextFill()
}

// nextFill is next when the buffer is exhausted.
func (s *v8Scanner) nextFill() (byte, bool) {
	if !s.fill() {
		return 0, false
	}
	s.i = 1
	return s.buf[0], true
}

// unread steps back over the byte next returned last (always in buf).
func (s *v8Scanner) unread() { s.i-- }

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// skipSpace returns the next byte that is not JSON whitespace.
func (s *v8Scanner) skipSpace() (byte, bool) {
	for {
		buf, i := s.buf[:s.n], s.i
		for i < len(buf) {
			c := buf[i]
			i++
			if !isJSONSpace(c) {
				s.i = i
				return c, true
			}
		}
		s.i = i
		if !s.fill() {
			return 0, false
		}
	}
}

// strPlain marks the bytes a JSON string holds as they are: not a quote, a
// backslash or a control character.
var strPlain = func() (t [256]bool) {
	for c := 0x20; c < 256; c++ {
		t[c] = c != '"' && c != '\\'
	}
	return t
}()

// plain skips the plain string bytes (strPlain) in the buffer.
func (s *v8Scanner) plain() {
	buf, i := s.buf[:s.n], s.i
	for i < len(buf) && strPlain[buf[i]] {
		i++
	}
	s.i = i
}

// isURLKey reports whether the raw text of an object key makes the byte scan smates
// used before v8Scanner match the bytes "url":" when a colon and a string follow it
// directly. In a valid document those bytes are the end of such a key: one of just
// url, or one ending in an escaped quote and url.
func isURLKey(raw []byte) bool {
	n := len(raw)
	return n >= 3 && raw[n-3] == 'u' && raw[n-2] == 'r' && raw[n-1] == 'l' &&
		(n == 3 || n >= 5 && raw[n-4] == '"' && raw[n-5] == '\\')
}

// value reads a value whose first byte c was read, inside depth arrays and objects.
func (s *v8Scanner) value(c byte, depth int) bool {
	switch {
	case c == '{':
		return depth < maxV8Depth && s.object(depth+1)
	case c == '[':
		return depth < maxV8Depth && s.array(depth+1)
	case c == '"':
		return s.string(nil)
	case c == '-' || c >= '0' && c <= '9':
		return s.number(c)
	case c == 't':
		return s.literal("rue")
	case c == 'f':
		return s.literal("alse")
	case c == 'n':
		return s.literal("ull")
	}
	return false
}

// object reads the rest of an object after its '{'.
func (s *v8Scanner) object(depth int) bool {
	c, ok := s.skipSpace()
	if ok && c == '}' {
		return true
	}
	for {
		if !ok || c != '"' {
			return false
		}
		var url bool
		start := s.i
		if s.plain(); s.i < s.n && s.buf[s.i] == '"' {
			// The whole key is in the buffer (the usual case): no copy.
			url = isURLKey(s.buf[start:s.i])
			s.i++
		} else {
			s.key = append(s.key[:0], s.buf[start:s.i]...)
			if !s.string(&s.key) {
				return false
			}
			url = isURLKey(s.key)
		}
		c, ok = s.next()
		direct := ok && c == ':'
		if !direct {
			if ok && isJSONSpace(c) {
				c, ok = s.skipSpace()
			}
			if !ok || c != ':' {
				return false
			}
		}
		if c, ok = s.next(); !ok {
			return false
		}
		if url && direct && c == '"' {
			s.str = s.str[:0]
			if !s.string(&s.str) {
				return false
			}
			s.addURL(s.str)
		} else {
			if isJSONSpace(c) {
				if c, ok = s.skipSpace(); !ok {
					return false
				}
			}
			if !s.value(c, depth) {
				return false
			}
		}
		c, ok = s.skipSpace()
		if !ok {
			return false
		}
		if c == '}' {
			return true
		}
		if c != ',' {
			return false
		}
		c, ok = s.skipSpace()
	}
}

// array reads the rest of an array after its '['.
func (s *v8Scanner) array(depth int) bool {
	c, ok := s.skipSpace()
	if ok && c == ']' {
		return true
	}
	for {
		if !ok || !s.value(c, depth) {
			return false
		}
		if c, ok = s.skipSpace(); !ok {
			return false
		}
		if c == ']' {
			return true
		}
		if c != ',' {
			return false
		}
		c, ok = s.skipSpace()
	}
}

// string reads the rest of a string after its opening quote, appending its raw bytes
// (escapes as written) to raw when it is not nil.
func (s *v8Scanner) string(raw *[]byte) bool {
	for {
		// Fast path: the run of plain bytes in the buffer.
		start := s.i
		s.plain()
		if raw != nil {
			*raw = append(*raw, s.buf[start:s.i]...)
		}
		c, ok := s.next()
		switch {
		case !ok || c < 0x20:
			return false
		case c == '"':
			return true
		case c == '\\':
			e, ok := s.next()
			if !ok {
				return false
			}
			if raw != nil {
				*raw = append(*raw, '\\', e)
			}
			switch e {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for k := 0; k < 4; k++ {
					h, ok := s.next()
					if !ok || !isHex(h) {
						return false
					}
					if raw != nil {
						*raw = append(*raw, h)
					}
				}
			default:
				return false
			}
		default:
			// A plain byte the fast path left: only at a buffer boundary.
			if raw != nil {
				*raw = append(*raw, c)
			}
		}
	}
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// number reads the rest of a number whose first byte c was read:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?. The byte after it is left unread.
func (s *v8Scanner) number(c byte) bool {
	var ok bool
	if c == '-' {
		if c, ok = s.next(); !ok {
			return false
		}
	}
	switch {
	case c == '0':
		c, ok = s.next()
	case c >= '1' && c <= '9':
		c, ok = s.digits()
	default:
		return false
	}
	if ok && c == '.' {
		if c, ok = s.next(); !ok || c < '0' || c > '9' {
			return false
		}
		c, ok = s.digits()
	}
	if ok && (c == 'e' || c == 'E') {
		if c, ok = s.next(); ok && (c == '+' || c == '-') {
			c, ok = s.next()
		}
		if !ok || c < '0' || c > '9' {
			return false
		}
		c, ok = s.digits()
	}
	if ok {
		s.unread()
	}
	return true
}

// digits skips digits and returns the first byte after them.
func (s *v8Scanner) digits() (byte, bool) {
	for {
		buf, i := s.buf[:s.n], s.i
		for i < len(buf) {
			c := buf[i]
			i++
			if c < '0' || c > '9' {
				s.i = i
				return c, true
			}
		}
		s.i = i
		if !s.fill() {
			return 0, false
		}
	}
}

// literal reads the rest of true, false or null.
func (s *v8Scanner) literal(rest string) bool {
	for k := 0; k < len(rest); k++ {
		if c, ok := s.next(); !ok || c != rest[k] {
			return false
		}
	}
	return true
}

// addURL adds the url value with raw text raw if it names a file: an absolute path,
// a file:// URL or a Windows path. Escapes are decoded as Go string literals, the raw
// text kept when that fails, as the byte scan did.
func (s *v8Scanner) addURL(raw []byte) {
	u := string(raw)
	if strings.IndexByte(u, '\\') >= 0 {
		if d, err := strconv.Unquote(`"` + u + `"`); err == nil {
			u = d
		}
	}
	if strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "/") || (len(u) > 2 && u[1] == ':') {
		s.urls = append(s.urls, u)
	}
}

// v8Job is one coverage file of a trace and what scanning it found.
type v8Job struct {
	path string
	p    *Proc
	urls []string
	err  error
	done chan struct{}
}

// scanV8Files scans the coverage files in parallel (bounded by GOMAXPROCS) and calls
// merge for each, in order, once it is scanned. At most twice as many files as
// workers are scanned ahead of merge, which bounds the URLs held in memory.
func scanV8Files(files []*v8Job, merge func(*v8Job)) {
	if len(files) == 0 {
		return
	}
	workers := min(runtime.GOMAXPROCS(0), len(files))
	ahead := make(chan struct{}, 2*workers)
	jobs := make(chan *v8Job)
	for range workers {
		go func() {
			for f := range jobs {
				f.urls, f.err = v8URLs(f.path)
				close(f.done)
			}
		}()
	}
	go func() {
		for _, f := range files {
			ahead <- struct{}{}
			jobs <- f
		}
		close(jobs)
	}()
	for _, f := range files {
		<-f.done
		merge(f)
		f.urls = nil
		<-ahead
	}
}
