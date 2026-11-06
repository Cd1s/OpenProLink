package enginedb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/OpenProLink/third_party/enginex/compat"
	"github.com/Cd1s/OpenProLink/third_party/enginex/rekordbox"
	"github.com/google/uuid"
)

type preparedTrack struct {
	track     rekordbox.Track
	rel, hash string
	info      os.FileInfo
	files     []analysisFile
	art       *analysisFileExport
	// broken is why the track's analysis or artwork could not be found (#53).
	broken error
}

func prepareTracks(ctx context.Context, res *compat.Resolver, l *rekordbox.Library) ([]preparedTrack, error) {
	prepared := make([]preparedTrack, 0, len(l.Tracks))
	paths := map[string]uint32{}
	for _, t := range l.Tracks {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		rel, e := res.Resolve(t.Path)
		if e != nil {
			return nil, fmt.Errorf("track %d: %w", t.ID, e)
		}
		if other, ok := paths[rel]; ok {
			return nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", other, t.ID, rel)
		}
		paths[rel] = t.ID
		st, e := os.Stat(res.FullPath(rel))
		if e != nil {
			return nil, e
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("audio not regular: %s", rel)
		}
		files, art, broken := sourceFiles(res, t)
		fp := fingerprint{Encoding: performanceEncoding, Record: t.RecordHash + ":" + rekordbox.RecordHash(t), ResolvedPath: rel, Size: st.Size(), MTime: st.ModTime().UnixNano(), Artwork: art}
		for _, f := range files {
			fp.Analysis = append(fp.Analysis, analysisFileExport{f.rel, f.size, f.mtime})
		}
		prepared = append(prepared, preparedTrack{t, rel, digest(fp), st, files, art, broken})
	}
	return prepared, nil
}

// exportFileInfo stands in for a stat in fast mode, from what the export recorded.
type exportFileInfo struct {
	os.FileInfo
	size int64
}

func (f exportFileInfo) Size() int64        { return f.size }
func (f exportFileInfo) ModTime() time.Time { return time.Time{} }

// prepareFast trusts the export's metadata without reading audio, but stores the
// spelling the volume actually uses: case and NFC/NFD differences are resolved
// from directory entries, as in a full import (#6). A path that cannot be
// resolved (missing file) keeps the export spelling; one that tries to leave
// the volume is dropped.
func prepareFast(res *compat.Resolver, l *rekordbox.Library) []preparedTrack {
	prepared := make([]preparedTrack, 0, len(l.Tracks))
	paths := map[string]bool{}
	for _, t := range l.Tracks {
		rel := strings.TrimLeft(path.Clean("/"+t.Path), "/")
		if rel == "" || t.Path == "" || strings.HasPrefix(rel, "PIONEER/") {
			continue
		}
		if resolved, e := res.Resolve(t.Path); e == nil {
			rel = resolved
		}
		if paths[rel] {
			continue
		}
		paths[rel] = true
		prepared = append(prepared, preparedTrack{track: t, rel: rel, hash: "fast:" + t.RecordHash, info: exportFileInfo{size: t.FileSize}})
	}
	return prepared
}

// Stage every moved owned row first, so even two source tracks swapping paths
// can retain their Engine IDs without violating Track's immediate UNIQUE(path).
// Both temporary paths and the final rename are in the import transaction.
func stageOwnedMoves(ctx context.Context, tx *sql.Tx, prepared []preparedTrack, stagedOnly bool) (map[uint32]int64, error) {
	targets := map[uint32]string{}
	for _, p := range prepared {
		targets[p.track.ID] = "../" + p.rel
	}
	rows, e := tx.QueryContext(ctx, `SELECT s.trackId,t.pdbImportKey,s.path FROM BridgeState s JOIN Track t ON t.id=s.trackId`)
	if e != nil {
		return nil, e
	}
	type move struct {
		id     int64
		source uint32
	}
	var moves []move
	var removed []int64
	owned := map[uint32]int64{}
	for rows.Next() {
		var id int64
		var source uint32
		var oldPath string
		if e = rows.Scan(&id, &source, &oldPath); e != nil {
			rows.Close()
			return nil, e
		}
		target, ok := targets[source]
		if !ok {
			if stagedOnly && source < 1<<24 {
				continue
			}
			removed = append(removed, id)
			continue
		}
		if _, duplicate := owned[source]; duplicate {
			rows.Close()
			return nil, fmt.Errorf("ambiguous bridge source ID %d", source)
		}
		owned[source] = id
		if target != oldPath {
			moves = append(moves, move{id, source})
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	// Release obsolete paths before moving or inserting their replacements.
	for _, id := range removed {
		for _, query := range []string{
			`DELETE FROM PlaylistEntity WHERE trackId=? AND (databaseUuid IS NULL OR databaseUuid='' OR databaseUuid=(SELECT uuid FROM Information))`,
			`DELETE FROM BridgeState WHERE trackId=?`,
			`DELETE FROM BridgeAnalysis WHERE trackId=?`,
			`DELETE FROM Track WHERE id=?`,
		} {
			if _, e = tx.ExecContext(ctx, query, id); e != nil {
				return nil, e
			}
		}
	}
	prefix := ".rb-bridge-pending-" + uuid.NewString() + "/"
	for _, m := range moves {
		temporary := prefix + strconv.FormatInt(m.id, 10)
		if _, e = tx.ExecContext(ctx, `UPDATE Track SET path=? WHERE id=?`, temporary, m.id); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint='' WHERE trackId=?`, temporary, m.id); e != nil {
			return nil, e
		}
	}
	return owned, nil
}
