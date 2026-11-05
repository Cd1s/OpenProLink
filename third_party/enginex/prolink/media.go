package prolink

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
)

const (
	pdbPath    = "PIONEER/rekordbox/export.pdb"
	maxPDB     = 64 << 20
	maxANLZ    = 16 << 20
	mediaFetch = 4 * time.Second // bounds how long Observe holds the discovery lock
)

// MediaCache reads a track's Rekordbox metadata and ANLZ waveform straight
// from the Pioneer player whose USB/SD holds it (#59), so the waveform shows
// on the Denon without that drive being plugged into the Denon. It keeps
// export.pdb and the ANLZ files of loaded tracks under Dir/<player>-<slot>/
// in the drive's own layout, and fetches export.pdb again only when its size
// or mtime changes (a different stick was inserted).
type MediaCache struct {
	Dir     string
	Enabled func() bool // checked on every new track; nil means on
	Portmap int         // tests only; 0 is UDP 111
}

func (m *MediaCache) resolve(dev Device, host net.IP) (*rekordbox.Analysis, string, string) {
	if m == nil || host == nil || dev.TrackID == 0 || (m.Enabled != nil && !m.Enabled()) {
		return nil, "", ""
	}
	export, err := SlotExport(dev.SourceSlot)
	if err != nil {
		return nil, "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), mediaFetch)
	defer cancel()
	c := &NFSClient{IP: host, Portmap: m.Portmap}
	root := filepath.Join(m.Dir, fmt.Sprintf("%d-%d", dev.SourcePlayer, dev.SourceSlot))
	local := filepath.Join(root, pdbPath)
	_, file, err := c.Lookup(ctx, export, "/"+pdbPath)
	if err != nil {
		return nil, "", ""
	}
	stamp := fmt.Sprintf("%d %d", file.Size, file.MTime)
	if old, _ := os.ReadFile(local + ".stamp"); string(old) != stamp {
		data, _, err := c.Fetch(ctx, export, "/"+pdbPath, maxPDB)
		if err != nil {
			return nil, "", ""
		}
		_ = os.RemoveAll(root) // ANLZ files of the previous stick
		if writeFile(local, data) != nil || writeFile(local+".stamp", []byte(stamp)) != nil {
			return nil, "", ""
		}
	}
	lib, err := rekordbox.ReadPDB(local)
	if err != nil || lib == nil {
		return nil, "", ""
	}
	for _, trk := range lib.Tracks {
		if trk.ID != dev.TrackID {
			continue
		}
		// The path comes from the remote PDB: only files under PIONEER/USBANLZ.
		rel := filepath.Clean(strings.TrimPrefix(trk.AnalysisPath, "/"))
		if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
			return nil, trk.Title, trk.Artist
		}
		base := strings.TrimSuffix(rel, filepath.Ext(rel))
		var merged *rekordbox.Analysis
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			path := filepath.Join(root, base+ext)
			if _, err := os.Stat(path); err != nil {
				data, _, err := c.Fetch(ctx, export, "/"+base+ext, maxANLZ)
				if err != nil || writeFile(path, data) != nil {
					continue // .EXT and .2EX are optional
				}
			}
			if a, err := rekordbox.ReadAnalysis(path); err == nil && a != nil {
				if merged == nil {
					merged = a
				} else {
					merged.Merge(a)
				}
			}
		}
		return merged, trk.Title, trk.Artist
	}
	return nil, "", ""
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
