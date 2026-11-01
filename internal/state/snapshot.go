package state

import (
	"slices"
	"sort"
	"strings"
)

// unrecordedPrefix starts a snapshot entry that only some suites have compared: a
// --suite run records a file the suites left out also own (or own alone) as
// "?<hash>@<suites>", the hash it saw and the suites it ran (MarkerSnap). A --suite run
// whose suites all saw that hash finds the file unchanged; any other run, a run over
// every suite included, finds it changed until it records the file itself. A bare "?"
// is an entry no suite has compared.
const unrecordedPrefix = "?"

type snapKind uint8

const (
	snapHash       snapKind = iota // a content hash ("" = absent)
	snapUnrecorded                 // "?": no suite has compared the file
	snapMarker                     // "?<hash>@<suites>"
)

// SnapValue is one entry of the file snapshot (State.Files): the content hash every
// suite has compared, or a marker of the suites that have seen which hash. Its string
// form (String, ParseSnap) is the on-disk format. The zero value is the absent hash.
type SnapValue struct {
	kind snapKind
	hash string   // the content hash, or the hash the marker's suites saw ("" = absent)
	seen []string // the marker's suites, as written
}

// ParseSnap reads a snapshot entry. A value starting with "?" is a marker: "?" alone
// is Unrecorded, otherwise the hash runs up to the first "@" and comma-separated suite
// names follow ("?<hash>" without "@" reads as a marker no suite has seen yet). Every
// other value is a content hash.
func ParseSnap(v string) SnapValue {
	rest, ok := strings.CutPrefix(v, unrecordedPrefix)
	switch {
	case !ok:
		return SnapValue{kind: snapHash, hash: v}
	case rest == "":
		return SnapValue{kind: snapUnrecorded}
	}
	hash, names, _ := strings.Cut(rest, "@")
	var seen []string
	if names != "" {
		seen = strings.Split(names, ",")
	}
	return SnapValue{kind: snapMarker, hash: hash, seen: seen}
}

// HashSnap is the entry of a file every suite has compared with content hash h
// ("" = absent, see Absent).
func HashSnap(h string) SnapValue { return SnapValue{kind: snapHash, hash: h} }

// UnrecordedSnap is the entry "?": no suite has compared the file.
func UnrecordedSnap() SnapValue { return SnapValue{kind: snapUnrecorded} }

// MarkerSnap is the entry saying that suites saw a file with hash ("" = absent). The
// suites are sorted and deduplicated.
func MarkerSnap(hash string, suites []string) SnapValue {
	s := slices.Clone(suites)
	sort.Strings(s)
	return SnapValue{kind: snapMarker, hash: hash, seen: slices.Compact(s)}
}

// String returns the on-disk form.
func (v SnapValue) String() string {
	switch v.kind {
	case snapUnrecorded:
		return unrecordedPrefix
	case snapMarker:
		return unrecordedPrefix + v.hash + "@" + strings.Join(v.seen, ",")
	}
	return v.hash
}

// Hash returns the content hash and whether v holds one. An Unrecorded entry (or a
// marker) holds none: the hash after the marker is only what --suite runs of other
// suites compare, never content the file's own suites have seen.
func (v SnapValue) Hash() (string, bool) {
	if v.kind != snapHash {
		return "", false
	}
	return v.hash, true
}

// Unrecorded reports whether v is "?" or a marker: only some suites (or none) have
// compared the file.
func (v SnapValue) Unrecorded() bool { return v.kind != snapHash }

// Absent reports whether v is the empty content hash: the file does not exist, and
// the snapshot holds no entry for it.
func (v SnapValue) Absent() bool { return v.kind == snapHash && v.hash == "" }

// SeenHash returns the hash the marker's suites saw ("" = absent) and whether v is
// Unrecorded (a bare "?" has seen the absent file, by no suite).
func (v SnapValue) SeenHash() (string, bool) {
	if v.kind == snapHash {
		return "", false
	}
	return v.hash, true
}

// SeenBy returns the suites of a marker (nil otherwise).
func (v SnapValue) SeenBy() []string { return slices.Clone(v.seen) }

// Seen reports whether suite has seen the marker's hash.
func (v SnapValue) Seen(suite string) bool { return slices.Contains(v.seen, suite) }

// SeenByAll reports whether every one of suites has seen the marker's hash (true for
// none).
func (v SnapValue) SeenByAll(suites []string) bool {
	for _, s := range suites {
		if !v.Seen(s) {
			return false
		}
	}
	return true
}

// MarkSeen returns the entry after suites saw the file with hash: the suites that
// already saw that same hash keep it, the others do not.
func (v SnapValue) MarkSeen(hash string, suites []string) SnapValue {
	if h, ok := v.SeenHash(); ok && h == hash {
		return MarkerSnap(hash, append(slices.Clone(v.seen), suites...))
	}
	return MarkerSnap(hash, suites)
}

// CollapseIfComplete returns the plain content hash of a marker every one of owners has
// seen (the file's suites have all compared it), v itself otherwise.
func (v SnapValue) CollapseIfComplete(owners []string) SnapValue {
	if v.kind != snapMarker || !v.SeenByAll(owners) {
		return v
	}
	return HashSnap(v.hash)
}

// Snap returns the snapshot entry for f and whether there is one.
func (s *State) Snap(f string) (SnapValue, bool) {
	v, ok := s.Files[f]
	return ParseSnap(v), ok
}

// FileHash returns the content hash the snapshot holds for f and whether it holds one
// (see SnapValue.Hash).
func (s *State) FileHash(f string) (string, bool) {
	v, ok := s.Snap(f)
	if !ok {
		return "", false
	}
	return v.Hash()
}

// SetSnap stores v as f's snapshot entry, or removes the entry when v is Absent.
func (s *State) SetSnap(f string, v SnapValue) {
	if v.Absent() {
		delete(s.Files, f)
		return
	}
	if s.Files == nil {
		s.Files = map[string]string{}
	}
	s.Files[f] = v.String()
}
