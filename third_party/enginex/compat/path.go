// Package compat resolves exported paths and reads audio headers without changing audio.
package compat

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type Resolver struct {
	root string
	mu   sync.Mutex
	dirs map[string]map[string][]string
}

func NewResolver(root string) (*Resolver, error) {
	r, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	r, e = filepath.EvalSymlinks(r)
	if e != nil {
		return nil, e
	}
	return &Resolver{root: r, dirs: map[string]map[string][]string{}}, nil
}

func canonical(s string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(s))) }

// CleanRelative accepts Pioneer volume-absolute paths, but rejects traversal.
func CleanRelative(s string) (string, error) {
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("path contains NUL")
	}
	for _, c := range strings.Split(s, "/") {
		if c == ".." {
			return "", fmt.Errorf("path traversal: %q", s)
		}
	}
	s = strings.TrimLeft(s, "/")
	s = path.Clean(s)
	if s == "." || strings.Contains(s, ":") {
		return "", fmt.Errorf("invalid volume path %q", s)
	}
	return s, nil
}

// Resolve builds a per-directory case/NFC index lazily. It reads directory
// entries, never audio data or the whole USB directory tree, and refuses symlinks.
func (r *Resolver) Resolve(s string) (string, error) {
	rel, e := CleanRelative(s)
	if e != nil {
		return "", e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := ""
	for _, component := range strings.Split(rel, "/") {
		// Always consult directory entries: case/normalization-insensitive lookup
		// may succeed for a spelling that is not the name stored on the volume.
		idx, ok := r.dirs[cur]
		if !ok {
			entries, err := os.ReadDir(filepath.Join(r.root, cur))
			if err != nil {
				return "", err
			}
			idx = map[string][]string{}
			for _, ent := range entries {
				idx[canonical(ent.Name())] = append(idx[canonical(ent.Name())], ent.Name())
			}
			r.dirs[cur] = idx
		}
		matches := idx[canonical(component)]
		// macOS rekordbox writes the export as the hidden ".PIONEER", while
		// export.pdb keeps volume paths like "/PIONEER/USBANLZ/...".
		if len(matches) == 0 && cur == "" && canonical(component) == "pioneer" {
			matches = idx[".pioneer"]
		}
		// FAT32/Windows strips trailing spaces and periods from folder names (1.x H019).
		if len(matches) == 0 {
			if trimmed := strings.TrimRight(canonical(component), " ."); trimmed != "" {
				for k, names := range idx {
					if strings.TrimRight(k, " .") == trimmed {
						matches = append(matches, names...)
					}
				}
			}
		}
		for _, name := range matches {
			if name == component {
				matches = []string{name}
				break
			}
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("resolve %q: %w", s, fs.ErrNotExist)
		}
		if len(matches) != 1 {
			return "", fmt.Errorf("ambiguous normalized path %q", s)
		}
		cur = filepath.Join(cur, matches[0])
		st, err := os.Lstat(filepath.Join(r.root, cur))
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink refused: %s", cur)
		}
	}
	return filepath.ToSlash(cur), nil
}

func (r *Resolver) FullPath(rel string) string { return filepath.Join(r.root, filepath.FromSlash(rel)) }

// SafeOutputDir creates a destination only after checking every ancestor. In
// particular an Engine Library symlink must never redirect writes into PIONEER.
func SafeOutputDir(root, rel string) (string, error) {
	r, e := NewResolver(root)
	if e != nil {
		return "", e
	}
	clean, e := CleanRelative(rel)
	if e != nil {
		return "", e
	}
	cur := r.root
	for _, part := range strings.Split(clean, "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			if err = os.Mkdir(cur, 0755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe output directory %s", cur)
		}
	}
	return cur, nil
}
