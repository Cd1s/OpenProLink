// Package rekordbox reads Pioneer exports without opening any source for writing.
package rekordbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

type Track struct {
	ID           uint32
	Path         string
	AnalysisPath string
	Title        string
	Artist       string
	Album        string
	Genre        string
	Label        string
	Composer     string
	Remixer      string
	Comment      string
	Key          string
	ArtworkPath  string
	BPM          float64
	Duration     float64 // seconds
	SampleCount  uint64  // exact container sample count, when available
	SampleRate   int
	BitDepth     int
	Bitrate      int
	Year         int
	Rating       int
	FileSize     int64
	RecordHash   string
}

type Playlist struct {
	ID, ParentID, SortOrder uint32
	Name                    string
	Folder, Intelligent     bool
	Rules                   string
	Tracks                  []uint32 // ordered, materialized membership, including intelligent playlists
}

type Library struct {
	Tracks    []Track
	Playlists []Playlist
	Artwork   map[uint32]string
	Keys      map[uint32]string
	Columns   [][]byte // unknown browse-column definitions retained verbatim
}

func RecordHash(t Track) string {
	t.RecordHash = ""
	b, _ := json.Marshal(t)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Validate rejects broken trees instead of silently losing subfolders or tracks.
func (l *Library) Validate() error {
	tracks := map[uint32]bool{}
	for i := range l.Tracks {
		t := &l.Tracks[i]
		if t.ID == 0 || tracks[t.ID] {
			return fmt.Errorf("rekordbox: duplicate/zero track id %d", t.ID)
		}
		tracks[t.ID] = true
		if t.Path == "" {
			return fmt.Errorf("rekordbox: track %d has no path", t.ID)
		}
		if math.IsNaN(t.BPM) || math.IsInf(t.BPM, 0) || t.BPM < 0 || t.BPM > 1000 || math.IsNaN(t.Duration) || math.IsInf(t.Duration, 0) || t.Duration < 0 || t.Duration > 1e7 || t.SampleRate < 0 || t.SampleRate > 768000 {
			return fmt.Errorf("rekordbox: invalid audio metadata for track %d", t.ID)
		}
		if t.RecordHash == "" {
			t.RecordHash = RecordHash(*t)
		}
	}
	parents := map[uint32]uint32{}
	names := map[string]bool{}
	for _, p := range l.Playlists {
		if _, ok := parents[p.ID]; p.ID == 0 || ok {
			return fmt.Errorf("rekordbox: duplicate/zero playlist id %d", p.ID)
		}
		parents[p.ID] = p.ParentID
		k := fmt.Sprintf("%d/%s", p.ParentID, p.Name)
		if names[k] {
			return fmt.Errorf("rekordbox: duplicate sibling playlist %q", p.Name)
		}
		names[k] = true
		for _, id := range p.Tracks {
			if !tracks[id] {
				return fmt.Errorf("rekordbox: playlist %d references absent track %d", p.ID, id)
			}
		}
	}
	for id := range parents {
		seen := map[uint32]bool{}
		for p := id; p != 0; p = parents[p] {
			if seen[p] {
				return fmt.Errorf("rekordbox: playlist cycle at %d", p)
			}
			if _, ok := parents[p]; !ok {
				return fmt.Errorf("rekordbox: missing parent %d", p)
			}
			seen[p] = true
		}
	}
	sort.SliceStable(l.Playlists, func(i, j int) bool {
		a, b := l.Playlists[i], l.Playlists[j]
		if a.ParentID != b.ParentID {
			return a.ParentID < b.ParentID
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID < b.ID
	})
	return nil
}
