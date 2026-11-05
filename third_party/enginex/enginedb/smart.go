package enginedb

import (
	"math"
	"strconv"
	"strings"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
)

// Camelot accepts wheel notation and common musical key spellings.
func Camelot(key string) (int, byte, bool) {
	k := strings.ToUpper(strings.TrimSpace(key))
	k = strings.ReplaceAll(strings.ReplaceAll(k, "♯", "#"), "♭", "B")
	if len(k) >= 2 {
		mode := k[len(k)-1]
		n, e := strconv.Atoi(k[:len(k)-1])
		if e == nil && n >= 1 && n <= 12 && (mode == 'A' || mode == 'B') {
			return n, mode, true
		}
	}
	minor := strings.HasSuffix(k, "M") || strings.HasSuffix(k, "MIN") || strings.HasSuffix(k, "MINOR") // case folded lowercase m is a minor suffix
	for _, suffix := range []string{"MINOR", "MAJOR", "MIN", "MAJ", "M"} {
		if strings.HasSuffix(k, suffix) {
			k = strings.TrimSpace(strings.TrimSuffix(k, suffix))
			break
		}
	}
	major := map[string]int{"B": 1, "F#": 2, "GB": 2, "DB": 3, "C#": 3, "AB": 4, "G#": 4, "EB": 5, "D#": 5, "BB": 6, "A#": 6, "F": 7, "C": 8, "G": 9, "D": 10, "A": 11, "E": 12}
	minors := map[string]int{"AB": 1, "G#": 1, "EB": 2, "D#": 2, "BB": 3, "A#": 3, "F": 4, "C": 5, "G": 6, "D": 7, "A": 8, "E": 9, "B": 10, "F#": 11, "GB": 11, "C#": 12, "DB": 12}
	if minor {
		n, ok := minors[k]
		return n, 'A', ok
	}
	n, ok := major[k]
	return n, 'B', ok
}

func CompatibleKeys(a, b string) bool {
	an, am, ok := Camelot(a)
	if !ok {
		return false
	}
	bn, bm, ok := Camelot(b)
	if !ok {
		return false
	}
	d := (an - bn + 12) % 12
	return (an == bn) || (am == bm && (d == 1 || d == 11))
}

// SmartMatch preserves source track order and excludes the currently playing track.
func SmartMatch(tracks []rekordbox.Track, current uint32) []uint32 {
	var ref *rekordbox.Track
	for i := range tracks {
		if tracks[i].ID == current {
			ref = &tracks[i]
			break
		}
	}
	if ref == nil || ref.BPM <= 0 {
		return nil
	}
	var ids []uint32
	for _, t := range tracks {
		if t.ID != current && t.BPM > 0 && math.Abs(t.BPM-ref.BPM) <= 4 && CompatibleKeys(t.Key, ref.Key) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// Engine interleaves relative major/minor keys around the circle of fifths:
// C=0, Am=1, G=2, Em=3, ... F=22, Dm=23 (libdjinterop musical_key).
func EngineKey(key string) any {
	n, m, ok := Camelot(key)
	if !ok {
		return nil
	}
	major := [12]int{10, 12, 14, 16, 18, 20, 22, 0, 2, 4, 6, 8}
	minor := [12]int{11, 13, 15, 17, 19, 21, 23, 1, 3, 5, 7, 9}
	if m == 'A' {
		return minor[n-1]
	}
	return major[n-1]
}
