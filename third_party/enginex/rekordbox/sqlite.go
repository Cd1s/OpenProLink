package rekordbox

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLiteAdapter permits vendor-version-specific OneLibrary readers without
// coupling the sync engine to a particular SQLite export schema or decryption.
type SQLiteAdapter interface {
	Read(context.Context, *sql.DB) (*Library, error)
}
type OneLibraryAdapter struct{}

func ReadSQLite(ctx context.Context, path string, adapter SQLiteAdapter) (*Library, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	u := url.URL{Scheme: "file", Path: abs}
	// mode=ro alone can still create/write a WAL shared-memory sidecar. Export
	// files must be clean snapshots: immutable avoids all source locks/writes,
	// and refusing journals prevents silently ignoring uncheckpointed changes.
	for _, suffix := range []string{"-wal", "-journal"} {
		st, err := os.Lstat(abs + suffix)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && (!st.Mode().IsRegular() || st.Size() > 0) {
			return nil, fmt.Errorf("OneLibrary: requires checkpointed export; source sidecar %s is present", suffix)
		}
	}
	db, e := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1&_pragma=query_only(1)")
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if adapter == nil {
		adapter = OneLibraryAdapter{}
	}
	l, e := adapter.Read(ctx, db)
	if e != nil {
		return nil, fmt.Errorf("OneLibrary SQLite (plaintext adapter required for encrypted exports): %w", e)
	}
	if e = l.Validate(); e != nil {
		return nil, e
	}
	return l, nil
}

type sqlRecord map[string]string

func normalized(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
}
func (r sqlRecord) get(names ...string) string {
	for _, n := range names {
		if v := r[normalized(n)]; v != "" {
			return v
		}
	}
	return ""
}
func (r sqlRecord) num(names ...string) uint32 {
	v, _ := strconv.ParseUint(r.get(names...), 10, 32)
	return uint32(v)
}
func (r sqlRecord) decimal(names ...string) float64 {
	v, _ := strconv.ParseFloat(r.get(names...), 64)
	return v
}

func readSQLRows(ctx context.Context, db *sql.DB, table string) ([]sqlRecord, error) {
	rows, e := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	cols, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	out := make([]sqlRecord, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = &values[i]
		}
		if e = rows.Scan(ptrs...); e != nil {
			return nil, e
		}
		r := sqlRecord{}
		for i, v := range values {
			switch v := v.(type) {
			case nil:
			case []byte:
				r[normalized(cols[i])] = string(v)
			default:
				r[normalized(cols[i])] = fmt.Sprint(v)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (OneLibraryAdapter) Read(ctx context.Context, db *sql.DB) (*Library, error) {
	rows, e := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if e != nil {
		return nil, e
	}
	tables := map[string]string{}
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			rows.Close()
			return nil, e
		}
		tables[normalized(s)] = s
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	load := func(names ...string) ([]sqlRecord, error) {
		for _, n := range names {
			if t := tables[normalized(n)]; t != "" {
				return readSQLRows(ctx, db, t)
			}
		}
		return nil, nil
	}
	l := &Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}
	lookups := map[string]map[uint32]string{}
	for _, kind := range []string{"artist", "album", "genre", "label", "key", "artwork", "image"} {
		rs, e := load(kind, "djmd"+kind)
		if e != nil {
			return nil, e
		}
		m := map[uint32]string{}
		for _, r := range rs {
			m[r.num("id", kind+"_id")] = r.get("name", "title", "path", "file_path")
		}
		lookups[kind] = m
	}
	l.Keys = lookups["key"]
	l.Artwork = lookups["artwork"]
	if len(l.Artwork) == 0 {
		l.Artwork = lookups["image"]
	}
	tracks, e := load("content", "djmdContent", "track", "tracks")
	if e != nil {
		return nil, e
	}
	if tracks == nil {
		return nil, fmt.Errorf("unsupported schema: missing content table")
	}
	for _, r := range tracks {
		path := r.get("file_path", "path", "location")
		if filename := r.get("file_name", "filename"); path == "" && filename != "" {
			path = strings.TrimRight(r.get("folder_path"), "/") + "/" + filename
		}
		bpm := r.decimal("bpmx100", "bpm", "tempo")
		if r.get("bpmx100") != "" || bpm > 1000 {
			bpm /= 100
		}
		key := r.get("key_name")
		if key == "" {
			key = lookups["key"][r.num("key_id")]
			if key == "" {
				key = r.get("key")
			}
		}
		t := Track{ID: r.num("id", "content_id", "track_id"), Path: path, AnalysisPath: r.get("analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path"), Title: r.get("title", "name"), Artist: lookups["artist"][r.num("artist_id_artist", "artist_id")], Composer: lookups["artist"][r.num("artist_id_composer", "composer_id")], Remixer: lookups["artist"][r.num("artist_id_remixer", "remixer_id")], Album: lookups["album"][r.num("album_id")], Genre: lookups["genre"][r.num("genre_id")], Label: lookups["label"][r.num("label_id")], Key: key, BPM: bpm, Duration: r.decimal("length", "duration"), SampleRate: int(r.num("samplingRate", "sample_rate")), BitDepth: int(r.num("bit_depth")), Bitrate: int(r.num("bit_rate", "bitrate")), FileSize: int64(r.decimal("file_size")), Year: int(r.num("release_year", "year")), Rating: int(r.num("rating")), Comment: r.get("djComment", "commnt", "comment"), ArtworkPath: l.Artwork[r.num("image_id", "artwork_id")]}
		if t.Artist == "" {
			t.Artist = r.get("artist")
		}
		if t.Album == "" {
			t.Album = r.get("album")
		}
		t.RecordHash = RecordHash(t)
		l.Tracks = append(l.Tracks, t)
	}
	ps, e := load("playlist", "djmdPlaylist")
	if e != nil {
		return nil, e
	}
	for _, r := range ps {
		attr := r.num("attribute")
		l.Playlists = append(l.Playlists, Playlist{ID: r.num("id", "playlist_id"), ParentID: r.num("playlist_id_parent", "parent_id"), SortOrder: r.num("sequenceNo", "seq", "sort_order"), Name: r.get("name", "title"), Folder: r.num("is_folder") != 0 || attr == 1, Intelligent: r.num("is_smart", "intelligent") != 0 || attr == 4, Rules: r.get("smart_list", "rules")})
	}
	es, e := load("playlist_content", "djmdSongPlaylist", "playlist_entry", "playlist_entries")
	if e != nil {
		return nil, e
	}
	// Sort by source sequence before materializing intelligent and ordinary lists.
	byID := map[uint32]int{}
	for i, p := range l.Playlists {
		byID[p.ID] = i
	}
	for _, r := range es {
		if _, ok := byID[r.num("playlist_id")]; !ok {
			return nil, fmt.Errorf("OneLibrary: orphan playlist entry")
		}
	}
	type entry struct{ seq, id uint32 }
	grouped := map[uint32][]entry{}
	for _, r := range es {
		id := r.num("playlist_id")
		grouped[id] = append(grouped[id], entry{r.num("sequenceNo", "track_no", "seq", "entry_index", "position"), r.num("content_id", "track_id")})
	}
	for i, p := range l.Playlists {
		xs := grouped[p.ID]
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].seq < xs[j].seq })
		for _, x := range xs {
			l.Playlists[i].Tracks = append(l.Playlists[i].Tracks, x.id)
		}
	}
	return l, nil
}
