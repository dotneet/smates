package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// v8URLsOracle is the byte scan smates used before v8Scanner: the whole file is read,
// checked with json.Valid, and searched for the bytes "url":" . The scanner must find
// exactly what it found, and reject exactly what it rejected.
func v8URLsOracle(data []byte) ([]string, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("truncated coverage file")
	}
	urlKey := []byte(`"url":"`)
	var urls []string
	for {
		i := bytes.Index(data, urlKey)
		if i < 0 {
			break
		}
		data = data[i+len(urlKey):]
		j := 0
		for j < len(data) && data[j] != '"' {
			if data[j] == '\\' {
				j++
			}
			j++
		}
		if j > len(data) {
			break
		}
		raw := data[:j]
		data = data[j:]
		s := string(raw)
		if bytes.IndexByte(raw, '\\') >= 0 {
			if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
				s = u
			}
		}
		if strings.HasPrefix(s, "file://") || strings.HasPrefix(s, "/") || (len(s) > 2 && s[1] == ':') {
			urls = append(urls, s)
		}
	}
	return urls, nil
}

// checkScan compares scanV8 on data with the oracle, with a few buffer sizes (so that
// every token sometimes straddles a refill) and with one-byte reads.
func checkScan(t *testing.T, data []byte) {
	t.Helper()
	want, wantErr := v8URLsOracle(data)
	for _, size := range []int{1, 2, 3, 7, 64, 64 << 10} {
		for _, oneByte := range []bool{false, true} {
			var r = bytes.NewReader(data)
			var got []string
			var err error
			if oneByte {
				got, err = scanV8(iotest.OneByteReader(r), make([]byte, size))
			} else {
				got, err = scanV8(r, make([]byte, size))
			}
			if (err != nil) != (wantErr != nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("scanV8(%q) with a %d-byte buffer (one-byte reads %v) = %q, %v; the byte scan gives %q, %v",
					data, size, oneByte, got, err, want, wantErr)
			}
		}
	}
}

func TestScanV8MatchesByteScan(t *testing.T) {
	for _, s := range v8Corpus() {
		checkScan(t, []byte(s))
	}
}

// v8Corpus are documents covering the JSON grammar, V8 coverage shapes and the corner
// cases of the byte scan.
func v8Corpus() []string {
	return []string{
		// Coverage shapes.
		`{"result":[{"scriptId":"1","url":"file:///a/b.js","functions":[{"functionName":"","ranges":[{"startOffset":0,"endOffset":10,"count":1}],"isBlockCoverage":false}]}],"timestamp":1234.5}`,
		`{"result":[{"url":"file:///a.js"},{"url":"node:fs"},{"url":""},{"url":"/b.ts"},{"url":"C:\\x.js"}],"source-map-cache":{"file:///a.js":{"lineLengths":[1,2],"data":{"version":3,"sources":["a.ts"]},"url":"file:///a.js.map"}}}`,
		`{"result":[{"url":"/a/q\"x.js"},{"url":"/a/\u00e9.js"},{"url":"/a\/b.js"},{"url":"/\ud83d\ude00.js"},{"url":"/a\\b"}]}`,
		// Not a file, or not a string.
		`{"url":null,"x":{"url":1},"y":[{"url":["/a"]}],"z":{"url":{"a":"/b"}}}`,
		// Space around the colon: the byte scan did not see these.
		`{"url" :"/a.js","b":{"url": "/b.js"},"c":{"url":` + "\n" + `"/c.js"}}`,
		// A key ending in an escaped quote and url.
		`{"x\"url":"/a.js","\"url":"/b.js","url\"":"/c.js","URL":"/d.js"}`,
		// "url" as a value, and keys and values that contain url.
		`{"a":"url","b":["url",":","/x"],"c":"\"url\":\"/no\""}`,
		// Whitespace.
		" \t\r\n{ \"url\":\"/a.js\" , \"b\" : [ 1 , 2 ] } \n",
		// Scalars and nesting.
		`[]`, `{}`, `[[[[]]]]`, `""`, `"\u0041"`, `0`, `-0`, `1.5e+10`, `-1E-2`, `12`, `true`, `false`, `null`,
		`[0,1,-2,3.25,4e5,6E-7,8.0e+9]`, `{"a":{"b":{"c":[true,false,null]}}}`,
		strings.Repeat("[", maxV8Depth) + strings.Repeat("]", maxV8Depth),
		strings.Repeat("[", maxV8Depth+1) + strings.Repeat("]", maxV8Depth+1),
		strings.Repeat(`{"a":`, maxV8Depth) + "1" + strings.Repeat("}", maxV8Depth),
		strings.Repeat(`{"a":`, maxV8Depth+1) + "1" + strings.Repeat("}", maxV8Depth+1),
		// Invalid documents.
		``, ` `, `{`, `}`, `[`, `]`, `{"a"}`, `{"a":}`, `{"a":1,}`, `[1,]`, `[,1]`, `{,}`, `{"a":1 "b":2}`, `{1:2}`,
		`[01]`, `[1.]`, `[.5]`, `[-]`, `[1e]`, `[1e+]`, `[+1]`, `[- 1]`, `[0x1]`, `[1]x`, `[1][2]`, `{}{}`, `nul`, `tru`, `fals`, `nulll`,
		`"a`, `"\x"`, `"\u12"`, `"\u12G4"`, "\"a\tb\"", "\"a\nb\"", "\"\x00\"", `"\`, `[" `,
		`{"result":[{"url":"/a.js"},{"url":"/b`, `{"url":"/a\`, `{"result":[{"url":"/a.js","functions":[{"ranges":[{"count":1}`,
		`{"url":"/a.js"`, `{"url":"/a.js",`, `{"url":"/a.js"}` + "\x00",
		// Bytes that are not UTF-8 (json.Valid accepts them in strings).
		"{\"url\":\"/a\xff\xfe.js\"}", "\"\x80\"",
		// Invalid escapes Go's Unquote rejects or reads differently than JSON.
		`{"url":"/a\'b"}`, `{"url":"/a\u0000b"}`,
	}
}

func FuzzScanV8(f *testing.F) {
	for _, s := range v8Corpus() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkScan(t, data)
	})
}

func TestScanV8ReadError(t *testing.T) {
	// The second read fails: the document is cut short by an error, not by its end.
	r := iotest.TimeoutReader(strings.NewReader(`{"result":[{"url":"/a.js"},{"url":"/b.js"}]}`))
	if _, err := scanV8(r, make([]byte, 4)); !errors.Is(err, iotest.ErrTimeout) {
		t.Errorf("a read error must fail the scan with it: %v", err)
	}
	// An error with the last bytes of a complete document still fails.
	r = iotest.ErrReader(errors.New("boom"))
	if _, err := scanV8(r, make([]byte, 4)); err == nil || errors.Is(err, errV8Invalid) {
		t.Errorf("read error: %v", err)
	}
}

// syntheticV8 returns a V8 coverage document with n scripts of fns functions each.
func syntheticV8(n, fns int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"result":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"scriptId":"%d","url":"file:///repo/src/m%d.ts","functions":[`, i, i)
		for j := 0; j < fns; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"functionName":"f%d","ranges":[{"startOffset":%d,"endOffset":%d,"count":%d},{"startOffset":%d,"endOffset":%d,"count":0}],"isBlockCoverage":true}`,
				j, j*10, j*10+200, j%3, j*10+5, j*10+9)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`],"timestamp":123456.789}`)
	return b.Bytes()
}

func TestScanV8Synthetic(t *testing.T) {
	data := syntheticV8(50, 40)
	want, err := v8URLsOracle(data)
	if err != nil || len(want) != 50 {
		t.Fatalf("oracle %d urls, %v", len(want), err)
	}
	got, err := scanV8(bytes.NewReader(data), make([]byte, 4096))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %d urls, %v", len(got), err)
	}
	// Truncated anywhere: rejected, as by json.Valid.
	for cut := 1; cut < len(data); cut += 97 {
		if _, err := scanV8(bytes.NewReader(data[:cut]), make([]byte, 4096)); err == nil {
			t.Fatalf("truncated at %d accepted", cut)
		}
	}
}

// TestCollectManyV8Files checks the parallel scan merges every file into the process
// it belongs to, skipping truncated ones, the same whatever the scheduling.
func TestCollectManyV8Files(t *testing.T) {
	root := repoDir(t)
	var paths []string
	for i := 0; i < 40; i++ {
		paths = append(paths, fmt.Sprintf("src/m%d.ts", i))
	}
	touch(t, root, paths...)
	td := t.TempDir()
	if err := os.MkdirAll(V8Dir(td), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		pid := 100 + i%4
		doc := fmt.Sprintf(`{"result":[{"url":"file://%s","functions":[]}]}`, filepath.ToSlash(filepath.Join(root, paths[i])))
		if i%10 == 9 {
			doc = doc[:len(doc)-3] // truncated: skipped
		}
		v8File(t, td, pid, int64(1000+i), 0, doc)
	}
	for run := 0; run < 3; run++ {
		procs, err := Collect(td, root)
		if err != nil {
			t.Fatal(err)
		}
		m := byKey(procs)
		for pid := 100; pid < 104; pid++ {
			p := m[fmt.Sprintf("%d-0", pid)]
			var want []string
			for i := pid - 100; i < 40; i += 4 {
				if i%10 != 9 {
					want = append(want, paths[i])
				}
			}
			sortStrings(want)
			if p == nil || !reflect.DeepEqual(keys(p.Code), want) {
				t.Fatalf("pid %d: %v, want %v", pid, p, want)
			}
		}
	}
}

func BenchmarkV8URLs(b *testing.B) {
	data := syntheticV8(200, 60)
	file := filepath.Join(b.TempDir(), "coverage-1-2-0.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		b.Fatal(err)
	}
	b.Run("stream", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			if _, err := v8URLs(file); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("scan-in-memory", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		buf := make([]byte, 64<<10)
		for b.Loop() {
			if _, err := scanV8(bytes.NewReader(data), buf); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("valid+scan-in-memory", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			if _, err := v8URLsOracle(data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("readfile+valid+scan", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			d, err := os.ReadFile(file)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := v8URLsOracle(d); err != nil {
				b.Fatal(err)
			}
		}
	})
}
