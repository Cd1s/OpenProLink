package bridge

import (
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/Cd1s/OpenProLink/third_party/enginex/prolink"
	"github.com/Cd1s/OpenProLink/third_party/enginex/rekordbox"
)

// stagedIDBits keeps staged IDs stable across imports: library k (in name
// order) takes IDs (k+1)<<24 | id, so its tracks keep their Engine identity
// while the drive's own export grows.
const stagedIDBits = 24

// stagedExports lists the libraries rb-bridge staged from Pioneer players into
// <mount>/.prolink-cache/<name>/ (#59), in name order.
func stagedExports(mount string) []string {
	dirs, _ := os.ReadDir(filepath.Join(mount, prolink.StagedDir))
	var names []string
	for _, d := range dirs {
		if d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			if _, err := os.Stat(filepath.Join(mount, prolink.StagedDir, d.Name(), "PIONEER", "rekordbox", "export.pdb")); err == nil {
				names = append(names, d.Name())
			}
		}
	}
	sort.Strings(names)
	return names
}

// stagedStamp changes whenever a staged library is written again.
func stagedStamp(mount string) string {
	s := ""
	for _, name := range stagedExports(mount) {
		if st, err := os.Stat(filepath.Join(mount, prolink.StagedDir, name, "PIONEER", "rekordbox", "export.pdb")); err == nil {
			s += fmt.Sprintf(" %s:%d:%d", name, st.Size(), st.ModTime().UnixNano())
		}
	}
	return s
}

// mergeStaged adds every staged library to l, each under a root folder
// "[Pro DJ Link: <name>]", with paths pointing into the staged copy.
// Reconcile then leaves out tracks whose audio has not been copied yet.
func mergeStaged(mount string, l *rekordbox.Library) {
	names := stagedExports(mount)
	if len(names) == 0 {
		return
	}
	var sortOrder uint32
	for _, t := range l.Tracks {
		if t.ID>>stagedIDBits != 0 {
			log.Printf("rb-bridge: track id %d too large to add Pro DJ Link libraries", t.ID)
			return
		}
	}
	for _, p := range l.Playlists {
		if p.ID>>stagedIDBits != 0 {
			log.Printf("rb-bridge: playlist id %d too large to add Pro DJ Link libraries", p.ID)
			return
		}
		if p.ParentID == 0 && p.SortOrder > sortOrder {
			sortOrder = p.SortOrder
		}
	}
	for k, name := range names {
		if k+1 >= 1<<(32-stagedIDBits) {
			break
		}
		staged, err := rekordbox.ReadPDB(filepath.Join(mount, prolink.StagedDir, name, "PIONEER", "rekordbox", "export.pdb"))
		if err != nil {
			log.Printf("rb-bridge: staged %s: %v", name, err)
			continue
		}
		base := uint32(k+1) << stagedIDBits
		// A broken staged export must never fail the drive's own import: build on a copy.
		merged := *l
		merged.Tracks = append([]rekordbox.Track(nil), l.Tracks...)
		merged.Playlists = append([]rekordbox.Playlist(nil), l.Playlists...)
		into := func(p string) string {
			rel, ok := prolink.SafeRel(p)
			if !ok {
				return ""
			}
			return "/" + path.Join(prolink.StagedDir, name, filepath.ToSlash(rel))
		}
		ids := map[uint32]bool{}
		for _, t := range staged.Tracks {
			if t.ID == 0 || t.ID>>stagedIDBits != 0 {
				continue
			}
			if t.Path = into(t.Path); t.Path == "" {
				continue
			}
			t.AnalysisPath, t.ArtworkPath = into(t.AnalysisPath), into(t.ArtworkPath)
			ids[t.ID] = true
			t.ID |= base
			t.RecordHash = ""
			merged.Tracks = append(merged.Tracks, t)
		}
		folder := base | (1<<stagedIDBits - 1)
		merged.Playlists = append(merged.Playlists, rekordbox.Playlist{ID: folder, SortOrder: sortOrder + 1, Name: "[Pro DJ Link: " + name + "]", Folder: true})
		for _, p := range staged.Playlists {
			if p.ID == 0 || p.ID>>stagedIDBits != 0 || p.ID == folder&^base || p.ParentID>>stagedIDBits != 0 {
				continue
			}
			p.ID |= base
			if p.ParentID == 0 {
				p.ParentID = folder
			} else {
				p.ParentID |= base
			}
			var tracks []uint32
			for _, id := range p.Tracks {
				if ids[id] {
					tracks = append(tracks, id|base)
				}
			}
			p.Tracks = tracks
			merged.Playlists = append(merged.Playlists, p)
		}
		if err := merged.Validate(); err != nil {
			log.Printf("rb-bridge: staged %s left out: %v", name, err)
			continue
		}
		*l = merged
		sortOrder++
	}
}
