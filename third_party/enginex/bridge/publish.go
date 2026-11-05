package bridge

// Fast imports work on a copy of the volume's database and bridge state and
// then put both back (#5). Three things make that safe:
//   - lockVolume holds the same per-volume lock a full import takes, for the
//     whole copy/import/publish, so no two imports interleave;
//   - snapshot copies through SQLite (VACUUM INTO), so committed WAL frames are
//     included and a half-written file is never read;
//   - publish stages both files and marks the stage complete before renaming
//     them in; finishPublish completes an interrupted publish on the next run,
//     so the database and its bridge state never come from different imports.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	publishDir   = ".rb-bridge-publish"
	publishReady = "READY"
)

func lockVolume(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".rb-bridge.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("bridge already syncing: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// snapshot writes a consistent single-file copy of the SQLite database src to
// dst. A missing src is not an error.
func snapshot(src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", src)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("snapshot %s: %w", filepath.Base(src), err)
	}
	return nil
}

// publish moves the named databases from work into dir as one unit.
func publish(work, dir string, names ...string) error {
	stage := filepath.Join(dir, publishDir)
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.Mkdir(stage, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		if err := snapshot(filepath.Join(work, name), filepath.Join(stage, name)); err != nil {
			return err
		}
		if err := syncFile(filepath.Join(stage, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(stage, publishReady), nil, 0o644); err != nil {
		return err
	}
	if err := syncFile(stage); err != nil {
		return err
	}
	return finishPublish(dir, names...)
}

// finishPublish completes a staged publish, or drops one that never became
// ready. Old WAL/journal sidecars go first: they belong to the replaced file
// and would otherwise be replayed into the new one.
func finishPublish(dir string, names ...string) error {
	stage := filepath.Join(dir, publishDir)
	if _, err := os.Stat(filepath.Join(stage, publishReady)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.RemoveAll(stage)
		}
		return err
	}
	for _, name := range names {
		staged := filepath.Join(stage, name)
		if _, err := os.Stat(staged); errors.Is(err, os.ErrNotExist) {
			continue // already moved in before an interruption
		}
		for _, side := range []string{"-wal", "-shm", "-journal"} {
			if err := os.Remove(filepath.Join(dir, name+side)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Rename(staged, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if err := syncFile(dir); err != nil {
		return err
	}
	return os.RemoveAll(stage)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
