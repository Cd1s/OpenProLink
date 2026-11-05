package enginedb

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/compat"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// stateFile holds bridge bookkeeping beside m.db. Engine compares m.db with its
// own schema on attach and drops every table it does not recognise, and then
// resets again on each attach if they return, so nothing of ours may live there.
const stateFile = ".rb-bridge-state.db"

const bridgeSchema = `
CREATE TABLE bridge.BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL);
CREATE TABLE bridge.BridgeAnalysis(trackId INTEGER PRIMARY KEY, sourceJSON BLOB NOT NULL, audioJSON BLOB NOT NULL);
CREATE TABLE bridge.BridgePlaylist(sourceId INTEGER PRIMARY KEY, listId INTEGER NOT NULL UNIQUE);
CREATE TABLE bridge.BridgeLibrary(key TEXT PRIMARY KEY, value BLOB);
`

var bridgeTables = []string{"BridgeState", "BridgeAnalysis", "BridgePlaylist", "BridgeLibrary"}

type Options struct {
	SmartMatch   bool
	CurrentTrack uint32
	// Fast imports only what the export itself holds, opening no audio, analysis
	// or artwork file. Tracks already imported are left as they are. Engine
	// analyses a track without performance data when it is loaded.
	Fast bool
	// DatabaseDir builds the database elsewhere, such as on tmpfs, instead of in
	// the mount's Engine Library.
	DatabaseDir string
	// Deadline stops reading audio and analysis files once it has passed. The
	// tracks left are imported as Fast imports them and counted in Deferred, so
	// the playlists still land while the mount waits (#48).
	Deadline time.Time `json:"-"`
}
type Result struct {
	Added, Updated, Unchanged int
	// Deferred tracks were imported without cues and waveforms: Deadline passed.
	Deferred         int `json:",omitempty"`
	PlaylistsChanged bool
	Bypassed         bool
}

// safeFile refuses redirection into PIONEER through symlinks or hard links.
func safeFile(p string) error {
	st, e := os.Lstat(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe database file %s", p)
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Nlink != 1 {
		return fmt.Errorf("hard-linked database file %s", p)
	}
	return nil
}

// playedIndicator returns the positive random 63-bit value Engine stores in
// Information.currentPlayedIndiciator.
func playedIndicator() int64 {
	id := uuid.New()
	return int64(binary.BigEndian.Uint64(id[:8])>>1) | 1
}

// open keeps schema initialization in the same transaction as the import.
func open(ctx context.Context, p string) (*sql.DB, *sql.Tx, error) {
	state := filepath.Join(filepath.Dir(p), stateFile)
	for _, file := range []string{p, state} {
		for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
			if e := safeFile(file + suffix); e != nil {
				return nil, nil, e
			}
		}
	}
	u := url.URL{Scheme: "file", Path: p}
	db, e := sql.Open("sqlite", u.String()+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if e != nil {
		return nil, nil, e
	}
	db.SetMaxOpenConns(1)
	var tx *sql.Tx
	success := false
	defer func() {
		if !success {
			if tx != nil {
				tx.Rollback()
			}
			db.Close()
		}
	}()
	// SetMaxOpenConns(1) keeps this attachment on the connection the import uses.
	// One transaction spans both files, so they commit atomically.
	if _, e = db.ExecContext(ctx, `ATTACH DATABASE ? AS bridge`, state); e != nil {
		return nil, nil, e
	}
	tx, e = db.BeginTx(ctx, nil)
	if e != nil {
		return nil, nil, e
	}
	var count int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&count); e != nil {
		return nil, nil, e
	}
	if count == 0 {
		if _, e = tx.ExecContext(ctx, schema); e != nil {
			return nil, nil, fmt.Errorf("engine schema: %w", e)
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch,currentPlayedIndiciator,lastRekordBoxLibraryImportReadCounter) VALUES(1,?,3,0,2,?,0)`, uuid.NewString(), playedIndicator())
	} else {
		var major, minor, patch, n int
		e = tx.QueryRowContext(ctx, `SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information`).Scan(&major, &minor, &patch)
		if e == nil {
			e = tx.QueryRowContext(ctx, `SELECT count(*) FROM Information`).Scan(&n)
		}
		if e == nil && (n != 1 || major != 3 || minor != 0 || patch != 2) {
			e = fmt.Errorf("unsupported Engine database schema %d.%d.%d", major, minor, patch)
		}
	}
	if e != nil {
		return nil, nil, e
	}
	// GL-010: Engine treats a library as corrupted if Information.id != 1 or uuid is empty.
	if _, e = tx.ExecContext(ctx, `UPDATE Information SET id=1 WHERE id<>1`); e != nil {
		return nil, nil, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE Information SET uuid=? WHERE uuid IS NULL OR trim(uuid)=''`, uuid.NewString()); e != nil {
		return nil, nil, e
	}
	// Earlier bridge builds left this NULL; Engine initialises it to a random value.
	if _, e = tx.ExecContext(ctx, `UPDATE Information SET currentPlayedIndiciator=? WHERE currentPlayedIndiciator IS NULL OR currentPlayedIndiciator=0`, playedIndicator()); e != nil {
		return nil, nil, e
	}
	var n int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE name='BridgeState'`).Scan(&n); e != nil {
		return nil, nil, e
	}
	if n == 0 {
		if _, e = tx.ExecContext(ctx, bridgeSchema); e != nil {
			return nil, nil, e
		}
	}
	// Libraries from earlier builds kept this state in m.db, not yet dropped by Engine.
	for _, table := range bridgeTables {
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM main.sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); e != nil {
			return nil, nil, e
		}
		if n == 0 {
			continue
		}
		if _, e = tx.ExecContext(ctx, fmt.Sprintf(`INSERT OR REPLACE INTO bridge.%[1]s SELECT * FROM main.%[1]s; DROP TABLE main.%[1]s`, table)); e != nil {
			return nil, nil, e
		}
	}
	if _, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO BridgeLibrary(key,value) VALUES('version','1')`); e != nil {
		return nil, nil, e
	}
	// The state file has no foreign keys into m.db: forget what Engine has deleted.
	for _, prune := range []string{
		`DELETE FROM BridgeState WHERE trackId NOT IN (SELECT id FROM main.Track)`,
		`DELETE FROM BridgeAnalysis WHERE trackId NOT IN (SELECT id FROM main.Track)`,
		`DELETE FROM BridgePlaylist WHERE listId NOT IN (SELECT id FROM main.Playlist)`,
	} {
		if _, e = tx.ExecContext(ctx, prune); e != nil {
			return nil, nil, e
		}
	}
	var v string
	if e = tx.QueryRowContext(ctx, `SELECT value FROM BridgeLibrary WHERE key='version'`).Scan(&v); e != nil {
		return nil, nil, e
	}
	if v != "1" {
		return nil, nil, fmt.Errorf("unsupported bridge schema %s", v)
	}
	success = true
	return db, tx, nil
}

type analysisFile struct {
	rel         string
	size, mtime int64
}

// performanceEncoding is part of each track's fingerprint: raising it re-encodes
// imported tracks' PerformanceData on their next import. 2 fixes beat spacing.
const performanceEncoding = 2

type fingerprint struct {
	Encoding     int
	Record       string
	ResolvedPath string
	Size, MTime  int64
	Analysis     []analysisFileExport
	Artwork      *analysisFileExport
}
type analysisFileExport struct {
	Path        string
	Size, MTime int64
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sourceFiles(res *compat.Resolver, t rekordbox.Track) ([]analysisFile, *analysisFileExport, error) {
	var files []analysisFile
	if t.AnalysisPath != "" {
		base := strings.TrimSuffix(t.AnalysisPath, path.Ext(t.AnalysisPath))
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			rel, e := res.Resolve(base + ext)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return nil, nil, e
			}
			s, e := os.Stat(res.FullPath(rel))
			if e != nil {
				return nil, nil, e
			}
			if !s.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("analysis not regular: %s", rel)
			}
			files = append(files, analysisFile{rel, s.Size(), s.ModTime().UnixNano()})
		}
		if len(files) == 0 {
			return nil, nil, fmt.Errorf("no analysis files for %s", t.AnalysisPath)
		}
	}
	var art *analysisFileExport
	if t.ArtworkPath != "" {
		rel, e := res.Resolve(t.ArtworkPath)
		if e != nil {
			return nil, nil, e
		}
		s, e := os.Stat(res.FullPath(rel))
		if e != nil {
			return nil, nil, e
		}
		if !s.Mode().IsRegular() || s.Size() > 32<<20 {
			return nil, nil, fmt.Errorf("artwork not a regular file within 32 MiB: %s", rel)
		}
		art = &analysisFileExport{rel, s.Size(), s.ModTime().UnixNano()}
	}
	return files, art, nil
}

// Sync imports only bridge-owned rows. All source files are opened read-only;
// the only output is Engine Library. Unchanged tracks do not read audio headers
// or ANLZ bodies and do not rewrite Track/PerformanceData.
func Sync(ctx context.Context, mount string, l *rekordbox.Library, opts Options) (result Result, err error) {
	if err = l.Validate(); err != nil {
		return result, err
	}
	res, err := compat.NewResolver(mount)
	if err != nil {
		return result, err
	}
	// The validated library may come entirely from staged CDJ media; its
	// receiver need not contain a local PIONEER directory. Bridge owns bypass.
	if opts.SmartMatch {
		found := false
		for _, t := range l.Tracks {
			found = found || t.ID == opts.CurrentTrack
		}
		if !found {
			return result, fmt.Errorf("Smart Match reference track %d not found", opts.CurrentTrack)
		}
	}
	var prepared []preparedTrack
	if opts.Fast {
		prepared = prepareFast(res, l)
	} else if prepared, err = prepareTracks(ctx, res, l); err != nil {
		return result, err
	}
	dir := opts.DatabaseDir
	if dir == "" {
		if dir, err = compat.SafeOutputDir(mount, "Engine Library/Database2"); err != nil {
			return result, err
		}
	}
	lockPath := filepath.Join(dir, ".rb-bridge.lock")
	if err = safeFile(lockPath); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, fmt.Errorf("bridge already syncing: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	db, tx, err := open(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		return result, err
	}
	defer db.Close()
	defer tx.Rollback()
	var databaseUUID string
	if err = tx.QueryRowContext(ctx, `SELECT uuid FROM Information`).Scan(&databaseUUID); err != nil {
		return result, err
	}
	mapping := map[uint32]int64{}
	effectiveTracks := append([]rekordbox.Track(nil), l.Tracks...)
	owned, err := stageOwnedMoves(ctx, tx, prepared)
	if err != nil {
		return result, err
	}
	for i, preparedTrack := range prepared {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		original := preparedTrack.track
		t := original
		rel, st, files, art, hash := preparedTrack.rel, preparedTrack.info, preparedTrack.files, preparedTrack.art, preparedTrack.hash
		full := res.FullPath(rel)
		enginePath := "../" + rel
		var id int64
		var previous string
		var storedBPM float64
		if ownedID := owned[t.ID]; ownedID != 0 {
			e := tx.QueryRowContext(ctx, `SELECT s.trackId,s.fingerprint,COALESCE(t.bpmAnalyzed,0) FROM BridgeState s JOIN Track t ON t.id=s.trackId WHERE s.trackId=?`, ownedID).Scan(&id, &previous, &storedBPM)
			if e != nil {
				return result, e
			}
		} else {
			e := tx.QueryRowContext(ctx, `SELECT s.trackId,s.fingerprint,COALESCE(t.bpmAnalyzed,0) FROM BridgeState s JOIN Track t ON t.id=s.trackId WHERE s.path=?`, enginePath).Scan(&id, &previous, &storedBPM)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return result, e
			}
		}
		fast := opts.Fast
		if !fast && previous != hash && !opts.Deadline.IsZero() && time.Now().After(opts.Deadline) {
			// A "fast:" fingerprint makes the next full import read this track.
			fast, files, art, hash = true, nil, nil, "fast:"+t.RecordHash
			result.Deferred++
		}
		existing := id != 0
		if !existing {
			// Engine (or its desktop app) already owns a track at this path:
			// reference it from imported playlists, but never adopt or rewrite it.
			var native int64
			e := tx.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=?`, enginePath).Scan(&native)
			if e == nil {
				mapping[t.ID] = native
				result.Unchanged++
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return result, e
			}
		}
		mapping[t.ID] = id
		// CF049: Rekordbox's analyzer can clamp misparsed AIFF files to 500.00 BPM
		// (or >499.5 BPM). Clear the bogus clamp before both Fast and Full paths.
		if t.BPM >= 499.5 {
			t.BPM = 0
		}
		if fast && existing {
			// An empty fingerprint is a row stageOwnedMoves just parked on a
			// temporary path; it must get its final path and metadata (#4).
			if previous != hash && (previous == "" || strings.HasPrefix(previous, "fast:")) {
				effectiveTracks[i] = t
				args := []any{int(t.Duration + 0.5), int(t.BPM + 0.5), t.Year, enginePath, path.Base(rel), t.Bitrate, t.BPM, st.Size(), t.Title, t.Artist, t.Album, t.Genre, t.Comment, t.Label, t.Composer, t.Remixer, EngineKey(t.Key), t.Rating, strings.TrimPrefix(strings.ToLower(path.Ext(rel)), "."), int64(t.ID), id}
				if _, e := tx.ExecContext(ctx, `UPDATE Track SET length=?,bpm=?,year=?,path=?,filename=?,bitrate=?,bpmAnalyzed=?,fileBytes=?,title=?,artist=?,album=?,genre=?,comment=?,label=?,composer=?,remixer=?,key=?,rating=?,fileType=?,pdbImportKey=? WHERE id=?`, args...); e != nil {
					return result, e
				}
				if _, e := tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint=? WHERE trackId=?`, enginePath, hash, id); e != nil {
					return result, e
				}
				result.Updated++
				continue
			}
			result.Unchanged++
			continue
		}
		if previous == hash {
			if t.BPM == 0 {
				effectiveTracks[i].BPM = storedBPM
			}
			result.Unchanged++
			continue
		}
		var e error
		a := &rekordbox.Analysis{}
		for _, f := range files {
			aa, e := rekordbox.ReadAnalysis(res.FullPath(f.rel))
			if e != nil {
				return result, e
			}
			a.Merge(aa)
		}
		var audio compat.AudioInfo
		if !fast {
			if audio, e = compat.AudioHeader(full); e != nil {
				return result, fmt.Errorf("track %d audio header: %w", t.ID, e)
			}
		}
		if audio.SampleRate > 0 {
			t.SampleRate = audio.SampleRate
			t.SampleCount = audio.Samples
			if audio.Duration > 0 {
				t.Duration = audio.Duration
			}
			t.BitDepth = audio.BitDepth
			if audio.Bitrate > 0 {
				t.Bitrate = audio.Bitrate
			}
		}
		if len(a.Beats) > 0 && a.Beats[0].BPM >= 499.5 {
			a.Beats = nil
		}
		if t.BPM == 0 && len(a.Beats) > 0 {
			t.BPM = a.Beats[0].BPM
		}
		effectiveTracks[i] = t
		// Compressing an empty performance is half of a fast import's time.
		var perf Performance
		if !fast {
			if perf, e = EncodePerformance(t, a); e != nil {
				return result, fmt.Errorf("track %d performance: %w", t.ID, e)
			}
		}
		var artID any
		if art != nil {
			if art.Size > 32<<20 {
				return result, fmt.Errorf("artwork exceeds 32 MiB")
			}
			f, e := os.Open(res.FullPath(art.Path))
			if e != nil {
				return result, e
			}
			data, e := io.ReadAll(io.LimitReader(f, 32<<20+1))
			f.Close()
			if e != nil {
				return result, e
			}
			if len(data) > 32<<20 {
				return result, fmt.Errorf("artwork exceeds 32 MiB")
			}
			h := sha1.Sum(data)
			artHash := hex.EncodeToString(h[:])
			var n int64
			e = tx.QueryRowContext(ctx, `SELECT id FROM AlbumArt WHERE hash=? LIMIT 1`, artHash).Scan(&n)
			if errors.Is(e, sql.ErrNoRows) {
				r, e := tx.ExecContext(ctx, `INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, artHash, data)
				if e != nil {
					return result, e
				}
				n, e = r.LastInsertId()
				if e != nil {
					return result, e
				}
			} else if e != nil {
				return result, e
			}
			artID = n
		}
		args := []any{int(t.Duration + 0.5), int(t.BPM + 0.5), t.Year, enginePath, path.Base(rel), t.Bitrate, t.BPM, artID, st.Size(), t.Title, t.Artist, t.Album, t.Genre, t.Comment, t.Label, t.Composer, t.Remixer, EngineKey(t.Key), t.Rating, strings.TrimPrefix(strings.ToLower(path.Ext(rel)), "."), perf.Complete, true, true, int64(t.ID)}
		if id == 0 {
			// A pre-existing native track at the same path is never adopted/overwritten.
			r, e := tx.ExecContext(ctx, `INSERT INTO Track(length,bpm,year,path,filename,bitrate,bpmAnalyzed,albumArtId,fileBytes,title,artist,album,genre,comment,label,composer,remixer,key,rating,fileType,isAnalyzed,isAvailable,isMetadataImported,pdbImportKey,dateCreated,dateAdded) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,strftime('%s'),strftime('%s'))`, args...)
			if e != nil {
				return result, fmt.Errorf("insert track %d: %w", t.ID, e)
			}
			id, e = r.LastInsertId()
			if e != nil {
				return result, e
			}
			result.Added++
		} else {
			args = append(args, id)
			_, e = tx.ExecContext(ctx, `UPDATE Track SET length=?,bpm=?,year=?,path=?,filename=?,bitrate=?,bpmAnalyzed=?,albumArtId=?,fileBytes=?,title=?,artist=?,album=?,genre=?,comment=?,label=?,composer=?,remixer=?,key=?,rating=?,fileType=?,isAnalyzed=?,isAvailable=?,isMetadataImported=?,pdbImportKey=? WHERE id=?`, args...)
			if e != nil {
				return result, e
			}
			result.Updated++
		}
		mapping[t.ID] = id
		// Track's factory trigger already inserted PerformanceData(trackId).
		// Fast mode leaves it empty, which is what makes Engine analyse on load.
		if !fast {
			if _, e = tx.ExecContext(ctx, `UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?,thirdPartySourceId=1,activeOnLoadLoops=? WHERE trackId=?`, perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops, perf.ActiveLoops, id); e != nil {
				return result, e
			}
		}
		if !fast {
			sourceJSON, _ := json.Marshal(struct {
				Track    rekordbox.Track
				Analysis *rekordbox.Analysis
			}{original, a})
			audioJSON, _ := json.Marshal(audio)
			if _, e = tx.ExecContext(ctx, `INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,?) ON CONFLICT(trackId) DO UPDATE SET sourceJSON=excluded.sourceJSON,audioJSON=excluded.audioJSON`, id, sourceJSON, audioJSON); e != nil {
				return result, e
			}
		}
		if existing {
			_, e = tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint=? WHERE trackId=?`, enginePath, hash, id)
		} else {
			_, e = tx.ExecContext(ctx, `INSERT INTO BridgeState(path,trackId,fingerprint) VALUES(?,?,?)`, enginePath, id, hash)
		}
		if e != nil {
			return result, e
		}
	}
	recommendations := SmartMatch(effectiveTracks, opts.CurrentTrack)
	playlistHash := digest(struct {
		Playlists       []rekordbox.Playlist
		Options         Options
		Mapping         map[uint32]int64
		Recommendations []uint32
	}{l.Playlists, opts, mapping, recommendations})
	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT value FROM BridgeLibrary WHERE key='playlists'`).Scan(&oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	expectedPlaylists := len(l.Playlists)
	if opts.SmartMatch {
		expectedPlaylists++
	}
	var actualPlaylists, validPlaylists int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),count(p.id) FROM BridgePlaylist b LEFT JOIN Playlist p ON p.id=b.listId`).Scan(&actualPlaylists, &validPlaylists); err != nil {
		return result, err
	}
	if oldHash != playlistHash || actualPlaylists != expectedPlaylists || validPlaylists != expectedPlaylists {
		if err = writePlaylists(ctx, tx, l, opts, mapping, databaseUUID, recommendations); err != nil {
			return result, err
		}
		result.PlaylistsChanged = true
		if _, err = tx.ExecContext(ctx, `INSERT INTO BridgeLibrary(key,value) VALUES('playlists',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, playlistHash); err != nil {
			return result, err
		}
	}
	if result.Added+result.Updated > 0 || result.PlaylistsChanged {
		if _, err = tx.ExecContext(ctx, `UPDATE Information SET lastRekordBoxLibraryImportReadCounter=COALESCE(lastRekordBoxLibraryImportReadCounter,0)+1`); err != nil {
			return result, err
		}
		metadata, _ := json.Marshal(struct {
			Columns   [][]byte
			Playlists []rekordbox.Playlist
		}{l.Columns, l.Playlists})
		if _, err = tx.ExecContext(ctx, `INSERT INTO BridgeLibrary(key,value) VALUES('source',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metadata); err != nil {
			return result, err
		}
	}
	// A staged move that no branch completed would commit an unloadable path (#4).
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM Track WHERE path LIKE '.rb-bridge-pending-%'`).Scan(&pending); err != nil {
		return result, err
	}
	if pending > 0 {
		return result, fmt.Errorf("%d tracks left on a temporary path; import rolled back", pending)
	}
	err = tx.Commit()
	return result, err
}

// uniqueTitle keeps Engine's UNIQUE(title,parentListId): a native playlist or
// a repeated rekordbox name under one folder gets a " (rekordbox N)" suffix.
func uniqueTitle(ctx context.Context, tx *sql.Tx, name string, parent int64) (string, error) {
	title := name
	for n := 1; ; n++ {
		var exists int
		if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE title=? AND parentListId=?`, title, parent).Scan(&exists); e != nil {
			return "", e
		}
		if exists == 0 {
			return title, nil
		}
		if n == 1 {
			title = name + " (rekordbox)"
		} else {
			title = fmt.Sprintf("%s (rekordbox %d)", name, n)
		}
	}
}

func writePlaylists(ctx context.Context, tx *sql.Tx, l *rekordbox.Library, opts Options, mapping map[uint32]int64, databaseUUID string, recommendations []uint32) error {
	// Refuse to cascade-delete an Engine-created child of an imported folder.
	var n int
	if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE parentListId IN (SELECT listId FROM BridgePlaylist) AND id NOT IN (SELECT listId FROM BridgePlaylist)`).Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return fmt.Errorf("imported folders contain native playlists; refusing destructive rebuild")
	}
	if _, e := tx.ExecContext(ctx, `DELETE FROM PlaylistEntity WHERE listId IN (SELECT listId FROM BridgePlaylist)`); e != nil {
		return e
	}
	// Delete leaves first: factory parent-delete triggers only cascade one level
	// when recursive_triggers is disabled, so explicitly remove the whole owned tree.
	rows, e := tx.QueryContext(ctx, `SELECT listId FROM BridgePlaylist ORDER BY listId DESC`)
	if e != nil {
		return e
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if _, e = tx.ExecContext(ctx, `DELETE FROM Playlist WHERE id=?`, id); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM BridgePlaylist`); e != nil {
		return e
	}
	if e = normalizePlaylistTails(ctx, tx); e != nil {
		return e
	}
	byParent := map[uint32][]rekordbox.Playlist{}
	for _, p := range l.Playlists {
		byParent[p.ParentID] = append(byParent[p.ParentID], p)
	}
	var insert func(uint32, int64) error
	insert = func(sourceParent uint32, parent int64) error {
		for _, p := range byParent[sourceParent] {
			// Engine can rebuild imported playlists under new ids, which orphans
			// BridgePlaylist. Take such a list back rather than adding a duplicate.
			var id int64
			e := tx.QueryRowContext(ctx, `SELECT id FROM Playlist WHERE title=? AND parentListId=? AND isExplicitlyExported=1 AND id NOT IN (SELECT listId FROM BridgePlaylist)`, p.Name, parent).Scan(&id)
			if e == nil {
				if _, e = tx.ExecContext(ctx, `DELETE FROM PlaylistEntity WHERE listId=?`, id); e != nil {
					return e
				}
			} else if errors.Is(e, sql.ErrNoRows) {
				title, e := uniqueTitle(ctx, tx, p.Name, parent)
				if e != nil {
					return e
				}
				r, e := tx.ExecContext(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(?,?,1,0,1)`, title, parent)
				if e != nil {
					return e
				}
				if id, e = r.LastInsertId(); e != nil {
					return e
				}
			} else {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO BridgePlaylist(sourceId,listId) VALUES(?,?)`, p.ID, id); e != nil {
				return e
			}
			if e = writeEntities(ctx, tx, id, p.Tracks, mapping, databaseUUID); e != nil {
				return e
			}
			if e = insert(p.ID, id); e != nil {
				return e
			}
		}
		return nil
	}
	if e = insert(0, 0); e != nil {
		return e
	}
	if opts.SmartMatch {
		// Never collide with an original playlist of the same name.
		var exists int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE parentListId=0 AND title='[Smart Match]'`).Scan(&exists); e != nil {
			return e
		}
		if exists > 0 {
			return fmt.Errorf("[Smart Match] already exists; source playlist is protected")
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES('[Smart Match]',0,1,0,1)`)
		if e != nil {
			return e
		}
		id, e := r.LastInsertId()
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO BridgePlaylist(sourceId,listId) VALUES(-1,?)`, id); e != nil {
			return e
		}
		return writeEntities(ctx, tx, id, recommendations, mapping, databaseUUID)
	}
	return nil
}

func writeEntities(ctx context.Context, tx *sql.Tx, list int64, tracks []uint32, mapping map[uint32]int64, databaseUUID string) error {
	// Engine membership is unique by track/list. Preserve first occurrence order.
	seen := map[uint32]bool{}
	var previous int64
	for _, sourceID := range tracks {
		if seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		id, ok := mapping[sourceID]
		if !ok || id == 0 {
			return fmt.Errorf("missing Engine mapping for track %d", sourceID)
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,0,0)`, list, id, databaseUUID)
		if e != nil {
			return e
		}
		entity, e := r.LastInsertId()
		if e != nil {
			return e
		}
		if previous != 0 {
			if _, e = tx.ExecContext(ctx, `UPDATE PlaylistEntity SET nextEntityId=? WHERE id=?`, entity, previous); e != nil {
				return e
			}
		}
		previous = entity
	}
	return nil
}
