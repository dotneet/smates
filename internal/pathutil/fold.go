package pathutil

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// relUnder returns p relative to root (both absolute and clean, symlinks resolved),
// false when p is not below root. On a file system that ignores case (macOS and Windows
// by default) a path can name a file in a case of its own: root is then matched
// ignoring case (/Users/me/Repo for /Users/me/repo, /PRIVATE/var for /private/var) and
// the part below it takes the case of the entries on disk (SRC/a.ts is src/a.ts), as
// git lists them. A part that does not exist (a probe) keeps the case it was given.
func relUnder(root, p string) (string, bool) {
	if !caseInsensitive(root) {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false
		}
		return rel, true
	}
	rest, ok := trimRootFold(root, p)
	if !ok {
		return "", false
	}
	return canonicalBelow(root, rest), true
}

// trimRootFold returns the part of p below root, comparing the elements of root with
// those of p ignoring case; false when p is root itself or not below it.
func trimRootFold(root, p string) (string, bool) {
	sep := string(filepath.Separator)
	rs := strings.Split(strings.TrimSuffix(root, sep), sep)
	ps := strings.Split(p, sep)
	if len(ps) <= len(rs) {
		return "", false
	}
	for i := range rs {
		if !strings.EqualFold(rs[i], ps[i]) {
			return "", false
		}
	}
	rest := ps[len(rs):]
	for _, e := range rest {
		if e == "" || e == "." || e == ".." {
			return "", false // not clean
		}
	}
	return strings.Join(rest, sep), true
}

// canonicalBelow gives rel, a path below dir, the case of the entries on disk, element
// by element; from the first element that does not exist it is kept as given.
func canonicalBelow(dir, rel string) string {
	sep := string(filepath.Separator)
	parts := strings.Split(rel, sep)
	for i, e := range parts {
		name, ok := entryName(dir, e)
		if !ok {
			break
		}
		parts[i] = name
		dir = filepath.Join(dir, name)
	}
	return strings.Join(parts, sep)
}

// caseFolds caches, by root, whether the file system of the root ignores case.
var caseFolds sync.Map // string -> bool

// caseInsensitive reports whether the file system holding dir ignores case: an entry
// of dir (or dir itself, or an ancestor) looked up with its case flipped is the same
// file. Unknown (no name with a letter, nothing readable) counts as case-sensitive.
func caseInsensitive(dir string) bool {
	if v, ok := caseFolds.Load(dir); ok {
		return v.(bool)
	}
	v := detectFold(dir)
	caseFolds.Store(dir, v)
	return v
}

func detectFold(dir string) bool {
	// probe looks p up with its last element's case flipped: decided is false when
	// that tells nothing (no letter in the name, p unreadable).
	probe := func(p string) (fold, decided bool) {
		base := filepath.Base(p)
		flipped := flipCase(base)
		if flipped == base {
			return false, false
		}
		a, err := os.Lstat(p)
		if err != nil {
			return false, false
		}
		b, err := os.Lstat(filepath.Join(filepath.Dir(p), flipped))
		if err != nil {
			return false, true
		}
		return os.SameFile(a, b), true
	}
	// An entry of dir is looked up in dir's own file system (dir may be a mount point).
	if f, err := os.Open(dir); err == nil {
		names, _ := f.Readdirnames(64)
		f.Close()
		for _, n := range names {
			if v, ok := probe(filepath.Join(dir, n)); ok {
				return v
			}
		}
	}
	for p := dir; p != filepath.Dir(p); p = filepath.Dir(p) {
		if v, ok := probe(p); ok {
			return v
		}
	}
	return false
}

// flipCase swaps the case of every letter of s.
func flipCase(s string) string {
	return strings.Map(func(r rune) rune {
		if u := unicode.ToUpper(r); u != r {
			return u
		}
		return unicode.ToLower(r)
	}, s)
}

// listing is the entries of a directory as last read (see entryName).
type listing struct {
	mtime time.Time
	// racy: read so soon after mtime that an entry created later may have kept it.
	racy  bool
	names map[string]bool
	lower map[string]string // lower-cased name -> name
}

// listings caches directory listings by directory.
var listings sync.Map // string -> *listing

func readListing(dir string) *listing {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil && len(names) == 0 {
		return nil
	}
	l := &listing{mtime: fi.ModTime(), racy: time.Since(fi.ModTime()) < 2*time.Second, names: make(map[string]bool, len(names)), lower: make(map[string]string, len(names))}
	for _, n := range names {
		l.names[n] = true
		l.lower[strings.ToLower(n)] = n
	}
	listings.Store(dir, l)
	return l
}

// lookup finds name among the entries, its case ignored when it is not there as is.
func (l *listing) lookup(name string) (string, bool) {
	if l.names[name] {
		return name, true
	}
	if n, ok := l.lower[strings.ToLower(name)]; ok {
		return n, true
	}
	for n := range l.names {
		if strings.EqualFold(n, name) {
			return n, true
		}
	}
	return "", false
}

// entryName returns the name of the entry of dir that name stands for ignoring case,
// as it is on disk; false when there is none. The listing is read again when name is
// not in it and the directory changed since (an entry created since).
func entryName(dir, name string) (string, bool) {
	var l *listing
	if v, ok := listings.Load(dir); ok {
		l = v.(*listing)
		if n, ok := l.lookup(name); ok {
			return n, true
		}
		if fi, err := os.Stat(dir); err != nil || !l.racy && fi.ModTime().Equal(l.mtime) {
			return "", false
		}
	}
	if l = readListing(dir); l == nil {
		return "", false
	}
	return l.lookup(name)
}
