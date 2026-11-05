package rekordbox

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

var le = binary.LittleEndian

// ParsePDB reads DeviceSQL pages through ReaderAt, keeping memory proportional to
// metadata rather than the on-disk database (which may contain deleted pages).
func ParsePDB(r io.ReaderAt, size int64) (*Library, error) {
	h := make([]byte, 28)
	if _, err := r.ReadAt(h, 0); err != nil {
		return nil, fmt.Errorf("pdb header: %w", err)
	}
	ps32, nt32 := le.Uint32(h[4:]), le.Uint32(h[8:])
	if ps32 < 512 || ps32 > 65536 || size%int64(ps32) != 0 || nt32 > (ps32-28)/16 {
		return nil, fmt.Errorf("rekordbox: invalid page size/table count")
	}
	ps, nt := int(ps32), int(nt32)
	tables := make([]byte, nt*16)
	if _, err := r.ReadAt(tables, 28); err != nil {
		return nil, err
	}
	l := &Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}
	type row struct {
		typ  uint32
		data []byte
	}
	var rows []row
	for ti := 0; ti < nt; ti++ {
		t := tables[ti*16:]
		typ, first, last := le.Uint32(t), le.Uint32(t[8:]), le.Uint32(t[12:])
		seen := map[uint32]bool{}
		for n := first; n != 0; {
			if seen[n] || int64(n) >= size/int64(ps) {
				return nil, fmt.Errorf("pdb: invalid/cyclic page %d", n)
			}
			seen[n] = true
			p := make([]byte, ps)
			if _, err := r.ReadAt(p, int64(n)*int64(ps)); err != nil {
				return nil, err
			}
			if le.Uint32(p[4:]) != n || le.Uint32(p[8:]) != typ {
				return nil, fmt.Errorf("pdb: page %d type/index mismatch", n)
			}
			if p[27]&0x40 == 0 {
				count := int(le.Uint32(p[24:]) & 0x1fff)
				// A full row group takes 36 bytes. The last one, if partial, only
				// has its flags and the offsets it uses.
				indexBytes := (count / 16) * 36
				if rem := count % 16; rem != 0 {
					indexBytes += 4 + 2*rem
				}
				heapEnd := ps - indexBytes
				if heapEnd < 40 {
					return nil, fmt.Errorf("pdb: row index overlaps header")
				}
				// Inactive offsets are stale and may alias a reused live allocation.
				// Never inspect them, including for row-boundary calculations.
				type ref struct {
					off  int
					live bool
				}
				var refs []ref
				for i := 0; i < count; i++ {
					base := ps - (i/16)*36
					off := 40 + int(le.Uint16(p[base-6-2*(i%16):]))
					live := le.Uint16(p[base-4:])&(1<<uint(i%16)) != 0
					if !live {
						continue
					}
					if off < 40 || off >= heapEnd {
						if live {
							return nil, fmt.Errorf("pdb: row outside heap")
						}
						continue
					}
					refs = append(refs, ref{off, live})
				}
				sort.Slice(refs, func(i, j int) bool { return refs[i].off < refs[j].off })
				for i, rf := range refs {
					if !rf.live {
						continue
					}
					end := heapEnd
					if i+1 < len(refs) {
						end = refs[i+1].off
					}
					if end <= rf.off {
						return nil, fmt.Errorf("pdb: overlapping row")
					}
					rows = append(rows, row{typ, append([]byte(nil), p[rf.off:end]...)})
				}
			}
			if n == last {
				break
			}
			n = le.Uint32(p[12:])
			if n == 0 {
				return nil, fmt.Errorf("pdb: page chain ends before last page")
			}
		}
	}
	artists, albums, genres, labels := map[uint32]string{}, map[uint32]string{}, map[uint32]string{}, map[uint32]string{}
	for _, r := range rows {
		b := r.data
		var id uint32
		var name string
		var err error
		switch r.typ {
		case 1, 4, 5, 13:
			if len(b) < 5 {
				return nil, fmt.Errorf("pdb: short lookup row")
			}
			id = le.Uint32(b)
			off := 4
			if r.typ == 5 {
				off = 8
			}
			name, err = readDeviceSQLString(b, off)
		case 2:
			if len(b) < 10 {
				return nil, fmt.Errorf("pdb: short artist row")
			}
			id = le.Uint32(b[4:])
			off := int(b[9])
			if le.Uint16(b)&4 != 0 {
				if len(b) < 12 {
					return nil, fmt.Errorf("pdb: short far artist")
				}
				off = int(le.Uint16(b[10:]))
			}
			name, err = readDeviceSQLString(b, off)
		case 3:
			if len(b) < 22 {
				return nil, fmt.Errorf("pdb: short album row")
			}
			id = le.Uint32(b[12:])
			off := int(b[21])
			if le.Uint16(b)&4 != 0 {
				if len(b) < 24 {
					return nil, fmt.Errorf("pdb: short far album")
				}
				off = int(le.Uint16(b[22:]))
			}
			name, err = readDeviceSQLString(b, off)
		case 16:
			l.Columns = append(l.Columns, append([]byte(nil), b...))
		}
		if err != nil {
			return nil, fmt.Errorf("pdb lookup %d: %w", r.typ, err)
		}
		switch r.typ {
		case 1:
			genres[id] = name
		case 2:
			artists[id] = name
		case 3:
			albums[id] = name
		case 4:
			labels[id] = name
		case 5:
			l.Keys[id] = name
		case 13:
			l.Artwork[id] = name
		}
	}
	type entry struct{ index, track uint32 }
	entries := map[uint32][]entry{}
	for _, r := range rows {
		b := r.data
		switch r.typ {
		case 0:
			if len(b) < 136 {
				return nil, fmt.Errorf("pdb: short track row")
			}
			ss := make([]string, 21)
			for i := range ss {
				off := int(le.Uint16(b[94+2*i:]))
				s, err := readDeviceSQLString(b, off)
				if err != nil {
					return nil, fmt.Errorf("pdb track string %d: %w", i, err)
				}
				ss[i] = s
			}
			// Row offsets do not encode sizes and the heap may have deleted holes.
			// Hash through the last referenced string, excluding unrelated padding.
			end := 136
			for i := 0; i < 21; i++ {
				off := int(le.Uint16(b[94+2*i:]))
				n := int(b[off] >> 1)
				if b[off]&1 == 0 {
					n = int(le.Uint16(b[off+1:]))
				}
				if off+n > end {
					end = off + n
				}
			}
			hash := sha256.Sum256(b[:end])
			l.Tracks = append(l.Tracks, Track{ID: le.Uint32(b[72:]), Path: ss[20], AnalysisPath: ss[14], Title: ss[17], Comment: ss[16], Artist: artists[le.Uint32(b[68:])], Album: albums[le.Uint32(b[64:])], Genre: genres[le.Uint32(b[60:])], Label: labels[le.Uint32(b[40:])], Composer: artists[le.Uint32(b[12:])], Remixer: artists[le.Uint32(b[44:])], Key: l.Keys[le.Uint32(b[32:])], ArtworkPath: l.Artwork[le.Uint32(b[28:])], SampleRate: int(le.Uint32(b[8:])), FileSize: int64(le.Uint32(b[16:])), Bitrate: int(le.Uint32(b[48:])), BPM: float64(le.Uint32(b[56:])) / 100, Year: int(le.Uint16(b[80:])), BitDepth: int(le.Uint16(b[82:])), Duration: float64(le.Uint16(b[84:])), Rating: int(b[89]), RecordHash: hex.EncodeToString(hash[:])})
		case 7:
			if len(b) < 21 {
				return nil, fmt.Errorf("pdb: short playlist")
			}
			name, err := readDeviceSQLString(b, 20)
			if err != nil {
				return nil, err
			}
			l.Playlists = append(l.Playlists, Playlist{ID: le.Uint32(b[12:]), ParentID: le.Uint32(b), SortOrder: le.Uint32(b[8:]), Folder: le.Uint32(b[16:]) != 0, Name: name})
		case 8:
			if len(b) < 12 {
				return nil, fmt.Errorf("pdb: short playlist entry")
			}
			id := le.Uint32(b[8:])
			entries[id] = append(entries[id], entry{le.Uint32(b), le.Uint32(b[4:])})
		}
	}
	for i := range l.Tracks { // include lookup metadata in the incremental fingerprint
		l.Tracks[i].RecordHash += ":" + RecordHash(l.Tracks[i])
	}
	for i := range l.Playlists {
		es := entries[l.Playlists[i].ID]
		delete(entries, l.Playlists[i].ID)
		sort.SliceStable(es, func(i, j int) bool { return es[i].index < es[j].index })
		for _, e := range es {
			l.Playlists[i].Tracks = append(l.Playlists[i].Tracks, e.track)
		}
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("pdb: entries reference missing playlists")
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

func ReadPDB(path string) (*Library, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return ParsePDB(f, s.Size())
}
