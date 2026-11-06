package prolink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/OpenProLink/third_party/enginex/rekordbox"
)

const maxAudio = 1 << 30

// StagedDir is where Stage puts players' libraries on a Denon drive; bridge
// imports what it finds there, under a "[Pro DJ Link: <name>]" folder (#59).
const StagedDir = ".prolink-cache"

// SafeRel turns a path from a remote export into one that stays inside its root.
func SafeRel(p string) (string, bool) {
	rel := filepath.Clean(strings.TrimPrefix(p, "/"))
	return rel, p != "" && filepath.IsLocal(rel)
}

// Stage copies a player's Rekordbox library from one media slot into dst, in
// the stick's own layout: every track's audio, ANLZ and artwork, then
// export.pdb last, so a half-finished copy never looks newer than the last
// complete one. Files already in dst are kept (they are only ever written
// whole). It stops once the copied bytes would pass max; tracks left without
// audio are skipped by the import. Returns the number of files copied.
func Stage(ctx context.Context, c *NFSClient, export, dst string, max int64) (int, error) {
	pdb, _, err := c.Fetch(ctx, export, "/"+pdbPath, maxPDB)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp("", "prolink-pdb-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(pdb)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	lib, err := rekordbox.ReadPDB(tmp.Name())
	if err != nil {
		return 0, err
	}
	copied, used := 0, int64(0)
	full := errors.New("full")
	fetch := func(remote string, limit uint32) error {
		rel, ok := SafeRel(remote)
		if !ok {
			return nil
		}
		local := filepath.Join(dst, rel)
		if _, err := os.Stat(local); err == nil {
			return nil
		}
		_, file, err := c.Lookup(ctx, export, "/"+filepath.ToSlash(rel))
		if err != nil {
			return nil // a file missing on the stick is skipped, as rekordbox does
		}
		if used+int64(file.Size) > max {
			return full
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(local), ".fetch-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		_, err = c.FetchTo(ctx, export, "/"+filepath.ToSlash(rel), f, limit)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := os.Rename(f.Name(), local); err != nil {
			return err
		}
		used += int64(file.Size)
		copied++
		return nil
	}
tracks:
	for _, t := range lib.Tracks {
		var files []string
		if base, ok := SafeRel(t.AnalysisPath); ok {
			base = strings.TrimSuffix(base, filepath.Ext(base))
			files = append(files, base+".DAT", base+".EXT", base+".2EX")
		}
		files = append(files, t.ArtworkPath, t.Path)
		for i, f := range files {
			limit := uint32(maxANLZ)
			if i == len(files)-1 {
				limit = maxAudio
			}
			if err := fetch(f, limit); err == full {
				break tracks
			} else if err != nil {
				return copied, err
			}
		}
		if ctx.Err() != nil {
			return copied, ctx.Err()
		}
	}
	return copied, writeFile(filepath.Join(dst, pdbPath), pdb)
}
