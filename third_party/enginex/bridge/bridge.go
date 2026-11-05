// Package bridge coordinates read-only export discovery and Engine imports.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/compat"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/enginedb"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ST_RDONLY from statvfs(3); syscall has no portable name for it.
const unix_ST_RDONLY = 0x1

var errNoLocalExport = fmt.Errorf("no local Rekordbox export: %w", os.ErrNotExist)

// ReadLibrary reads the drive's own export plus the libraries staged from
// Pioneer players on the LAN (#59).
func ReadLibrary(ctx context.Context, mount string) (*rekordbox.Library, error) {
	l, _, err := readLibrary(ctx, mount)
	return l, err
}
func readLibrary(ctx context.Context, mount string) (*rekordbox.Library, bool, error) {
	l, err := readExport(ctx, mount)
	stagedOnly := errors.Is(err, errNoLocalExport)
	if stagedOnly {
		l = &rekordbox.Library{}
		err = nil
	}
	if err != nil {
		return nil, false, err
	}
	if l != nil {
		mergeStaged(mount, l)
	}
	if stagedOnly && len(l.Tracks) == 0 && len(l.Playlists) == 0 {
		return nil, false, os.ErrNotExist
	}
	return l, stagedOnly, nil
}

func readExport(ctx context.Context, mount string) (*rekordbox.Library, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return nil, e
	}
	for _, p := range []string{"PIONEER/rekordbox/export.pdb", "PIONEER/rekordbox/exportLibrary.db", "PIONEER/rekordbox/exportOneLibrary.db"} {
		rel, e := r.Resolve(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if strings.EqualFold(filepath.Ext(rel), ".pdb") {
			return rekordbox.ReadPDB(r.FullPath(rel))
		}
		return rekordbox.ReadSQLite(ctx, r.FullPath(rel), nil)
	}
	return nil, errNoLocalExport
}

// ensureWritableEngineLibrary mounts an ext4-backed overlay onto "<mount>/Engine Library"
// when the underlying volume (such as macOS HFS+) is mounted read-only.
func ensureWritableEngineLibrary(mount string) bool {
	libDir := filepath.Join(mount, "Engine Library")
	var libSt syscall.Statfs_t
	if syscall.Statfs(libDir, &libSt) == nil && libSt.Flags&unix_ST_RDONLY == 0 {
		return true
	}
	info, err := os.Lstat(libDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if st, err := os.Stat("/data/sc5000-custom"); err != nil || !st.IsDir() {
		return false
	}
	base := filepath.Base(filepath.Clean(mount))
	if base == "" || base == "." || base == "/" {
		return false
	}
	lower := "/run/sc5000-empty"
	upper := filepath.Join("/data/sc5000-custom/hfs-lib", base, "upper")
	work := filepath.Join("/data/sc5000-custom/hfs-lib", base, "work")
	for _, d := range []string{lower, upper, work} {
		if os.MkdirAll(d, 0755) != nil {
			return false
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if syscall.Mount("overlay", libDir, "overlay", 0, opts) != nil {
		return false
	}
	return syscall.Statfs(libDir, &libSt) == nil && libSt.Flags&unix_ST_RDONLY == 0
}

func writable(mount string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(mount, &st) != nil || st.Flags&unix_ST_RDONLY == 0 || ensureWritableEngineLibrary(mount)
}

func Convert(ctx context.Context, mount string, opts enginedb.Options) (enginedb.Result, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return enginedb.Result{}, e
	}
	_, e = r.Resolve("PIONEER")
	if errors.Is(e, os.ErrNotExist) {
		if len(stagedExports(mount)) == 0 {
			return enginedb.Result{Bypassed: true}, nil
		}
		e = nil // a pure Engine/empty drive can still receive staged CDJ libraries
	}
	// Read-only volumes (HFS+ on Engine OS 5.0.4: rw writes crash hfsplus.ko)
	// cannot hold an Engine Library unless Engine Library itself is a writable mount.
	if e == nil && !writable(mount) {
		return enginedb.Result{Bypassed: true}, nil
	}
	if e != nil {
		return enginedb.Result{}, e
	}
	l, stagedOnly, e := readLibrary(ctx, mount)
	if errors.Is(e, os.ErrNotExist) {
		return enginedb.Result{Bypassed: true}, nil
	}
	if e != nil {
		return enginedb.Result{}, e
	}
	opts.StagedOnly = stagedOnly
	Reconcile(r, l)
	return enginedb.Sync(ctx, mount, l, opts)
}

// Reconcile fits a real export to what a library can hold, so that one odd entry
// cannot lose thousands of tracks. rekordbox can list one file under two records,
// and files get deleted from a stick after export. The first record of a file
// stands for it in playlists; a track whose file is gone is left out.
func Reconcile(r *compat.Resolver, l *rekordbox.Library) {
	byPath := map[string]uint32{}
	replaced := map[uint32]uint32{} // 0 means left out
	kept := l.Tracks[:0:0]
	var merged, missing int
	for _, t := range l.Tracks {
		rel, err := r.Resolve(t.Path)
		if err == nil {
			_, err = os.Stat(r.FullPath(rel))
		}
		if errors.Is(err, os.ErrNotExist) {
			replaced[t.ID] = 0
			missing++
			continue
		}
		if err != nil {
			kept = append(kept, t) // Sync reports anything else
			continue
		}
		if first, ok := byPath[rel]; ok {
			replaced[t.ID] = first
			merged++
			continue
		}
		byPath[rel] = t.ID
		kept = append(kept, t)
	}
	if len(replaced) == 0 {
		return
	}
	l.Tracks = kept
	replacePlaylistTracks(l, replaced)
	log.Printf("rb-bridge: %d duplicate records merged, %d tracks without a file left out", merged, missing)
}

const (
	stampName = ".rb-bridge-stamp"
	stateName = ".rb-bridge-state.db"
	// Past this, a first import cannot finish while the mount waits: reading every
	// file took 8 minutes for 6000 tracks on a USB stick.
	fastImportTracks = 500
	// The edisksd hook kills rb-bridge 20 s after the mount, which rolled back the
	// whole import, playlists included (#48). Stop reading files after this; the
	// rest of the tracks finish on later mounts.
	mountReadBudget = 8 * time.Second
)

// ConvertAtMount is Convert for when a volume is mounted, while Engine is waiting
// for it. An export unchanged since the last import is skipped at once. A large
// library's first import takes only what the export holds, built on tmpfs and
// copied over in one go. full forces a complete import, with cues and waveforms.
// Otherwise files are read for mountReadBudget at most, and the export counts as
// imported only once no track was left without its cues.
func ConvertAtMount(ctx context.Context, mount string, opts enginedb.Options, full bool) (enginedb.Result, error) {
	if !full && opts.Deadline.IsZero() {
		opts.Deadline = time.Now().Add(mountReadBudget)
	}
	r, err := compat.NewResolver(mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	var rel string
	for _, p := range []string{"PIONEER/rekordbox/export.pdb", "PIONEER/rekordbox/exportLibrary.db", "PIONEER/rekordbox/exportOneLibrary.db"} {
		if resolved, e := r.Resolve(p); e == nil {
			rel = resolved
			break
		}
	}
	var stamp string
	if rel != "" {
		st, err := os.Stat(r.FullPath(rel))
		if err != nil {
			return Convert(ctx, mount, opts)
		}
		stamp = fmt.Sprintf("%s %d %d", rel, st.Size(), st.ModTime().UnixNano()) + stagedStamp(mount)
	} else {
		stamp = strings.TrimSpace(stagedStamp(mount))
		if stamp == "" {
			return Convert(ctx, mount, opts)
		}
	}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if !full && !opts.SmartMatch && importedStamp(dir) == stamp {
		return enginedb.Result{Bypassed: true}, nil
	}
	var result enginedb.Result
	// GL-011: Always use fast tmpfs delta import at mount when a library exceeds
	// fastImportTracks, even if .rb-bridge-state.db already exists from an earlier mount.
	if !full && !opts.SmartMatch {
		l, stagedOnly, err := readLibrary(ctx, mount)
		if err != nil {
			return result, err
		}
		opts.StagedOnly = stagedOnly
		if len(l.Tracks) > fastImportTracks {
			opts.Fast = true
			result, err = stagedImport(ctx, mount, dir, l, opts)
			if err == nil {
				writeStamp(dir, stamp)
			}
			return result, err
		}
		// A first import that failed or was killed in place left an empty m.db,
		// and Engine then showed the stick as an empty Engine library instead of
		// offering its own rekordbox import (#53). Build it aside; publish or nothing.
		if !hasLibrary(dir) {
			if !writable(mount) {
				return enginedb.Result{Bypassed: true}, nil
			}
			Reconcile(r, l)
			result, err = stagedImport(ctx, mount, dir, l, opts)
			if err != nil {
				// Leave no Engine Library for Engine to mistake for one; Remove spares non-empty dirs.
				// An empty m.db is what an earlier build left behind on a failed import.
				for _, name := range []string{"m.db", stateName} {
					if st, e := os.Stat(filepath.Join(dir, name)); e == nil && st.Size() == 0 {
						os.Remove(filepath.Join(dir, name))
					}
				}
				os.Remove(filepath.Join(dir, ".rb-bridge.lock"))
				os.Remove(dir)
				os.Remove(filepath.Dir(dir))
			} else if result.Deferred == 0 {
				writeStamp(dir, stamp)
			}
			return result, err
		}
	}
	result, err = Convert(ctx, mount, opts)
	if err == nil && result.Deferred > 0 {
		log.Printf("rb-bridge: %d tracks imported without cues; the next mount reads them", result.Deferred)
	} else if err == nil && !result.Bypassed {
		writeStamp(dir, stamp)
	}
	return result, err
}

func importedStamp(dir string) string {
	if st, err := os.Stat(filepath.Join(dir, "m.db")); err != nil || st.Size() == 0 {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(dir, stampName))
	return strings.TrimSpace(string(b))
}

// hasLibrary reports a non-empty m.db, whatever wrote it.
func hasLibrary(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "m.db"))
	return err == nil && st.Size() > 0
}

func writeStamp(dir, stamp string) {
	if err := os.WriteFile(filepath.Join(dir, stampName), []byte(stamp+"\n"), 0o644); err != nil {
		log.Printf("rb-bridge: could not record the import: %v", err)
	}
}

// stagedImport syncs a copy of the volume's database in a temporary directory
// and publishes it back only if the whole import succeeded.
func stagedImport(ctx context.Context, mount, dir string, l *rekordbox.Library, opts enginedb.Options) (enginedb.Result, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return enginedb.Result{}, err
	}
	unlock, err := lockVolume(dir)
	if err != nil {
		return enginedb.Result{}, err
	}
	defer unlock()
	// An earlier run may have died between the two renames.
	if err = finishPublish(dir, stateName, "m.db"); err != nil {
		return enginedb.Result{}, err
	}
	work, err := os.MkdirTemp("", "rb-bridge-*")
	if err != nil {
		return enginedb.Result{}, err
	}
	defer os.RemoveAll(work)
	// Tracks and bridge state already held on this volume stay in the library for incremental sync.
	for _, name := range []string{"m.db", stateName} {
		if err = snapshot(filepath.Join(dir, name), filepath.Join(work, name)); err != nil {
			return enginedb.Result{}, err
		}
	}
	opts.DatabaseDir = work
	mergeByPath(l)
	result, err := enginedb.Sync(ctx, mount, l, opts)
	if err != nil {
		return result, err
	}
	return result, publish(work, dir, stateName, "m.db")
}

// mergeByPath is Reconcile without touching the disk.
func mergeByPath(l *rekordbox.Library) {
	byPath := map[string]uint32{}
	replaced := map[uint32]uint32{}
	kept := l.Tracks[:0:0]
	for _, t := range l.Tracks {
		// Same equivalence as compat's resolver: case folding and NFC (#6).
		key := norm.NFC.String(cases.Fold().String(norm.NFC.String(t.Path)))
		if first, ok := byPath[key]; ok {
			replaced[t.ID] = first
			continue
		}
		byPath[key] = t.ID
		kept = append(kept, t)
	}
	l.Tracks = kept
	replacePlaylistTracks(l, replaced)
}

func replacePlaylistTracks(l *rekordbox.Library, replaced map[uint32]uint32) {
	if len(replaced) == 0 {
		return
	}
	for i := range l.Playlists {
		var tracks []uint32
		for _, id := range l.Playlists[i].Tracks {
			first, changed := replaced[id]
			if !changed {
				tracks = append(tracks, id)
			} else if first != 0 {
				tracks = append(tracks, first)
			}
		}
		l.Playlists[i].Tracks = tracks
	}
}
