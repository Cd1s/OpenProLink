// Package prolink provides passive Pro DJ Link discovery/status and read-only
// access to players' media (nfs.go). It does not impersonate a CDJ or write to Rekordbox.
package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
)

const magic = "Qspt1WmJOL"

type BeatGridPoint struct {
	Beat   uint16  `json:"beat"`
	BPM    float64 `json:"bpm"`
	TimeMS uint32  `json:"time_ms"`
}

type Device struct {
	Address                  string `json:"address"`
	Name                     string `json:"name"`
	Number                   byte   `json:"number"`
	PacketType               byte   `json:"packet_type"`
	TrackID                  uint32 `json:"track_id,omitempty"`
	SourcePlayer, SourceSlot byte
	LastSeen                 time.Time `json:"last_seen"`
	BPM                      float64   `json:"bpm,omitempty"`
	Pitch                    float64   `json:"pitch,omitempty"`
	EffectiveBPM             float64   `json:"effective_bpm,omitempty"`
	Beat                     uint32    `json:"beat,omitempty"`
	BeatInBar                byte      `json:"beat_in_bar,omitempty"`
	PlayPositionMS           uint32    `json:"play_position_ms,omitempty"`
	PrecisePosition          bool      `json:"precise_position,omitempty"`
	lastPrecise              time.Time
	Playing                  bool            `json:"playing,omitempty"`
	OnAir                    bool            `json:"on_air,omitempty"`
	Master                   bool            `json:"master,omitempty"`
	Sync                     bool            `json:"sync,omitempty"`
	Title                    string          `json:"title,omitempty"`
	Artist                   string          `json:"artist,omitempty"`
	DurationMS               uint32          `json:"duration_ms,omitempty"`
	Waveform                 [][3]byte       `json:"waveform,omitempty"`
	Beats                    []BeatGridPoint `json:"beats,omitempty"`
}

// ParsePacket only accepts known packet shapes on their corresponding ports.
// Packet offsets follow Deep Symmetry's DJ Link protocol analysis.
func ParsePacket(b []byte, port int, remote net.IP) (Device, error) {
	d := Device{}
	if len(b) < 0x21 || string(b[:10]) != magic {
		return d, fmt.Errorf("prolink: invalid packet header")
	}
	d.PacketType = b[10]
	if remote != nil {
		d.Address = remote.String()
	}
	nameOffset := 11
	if port == 50000 {
		nameOffset = 12
	}
	name := b[nameOffset : nameOffset+20]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i] // C string: bytes after the NUL are buffer garbage (#36)
	}
	d.Name = strings.TrimRight(string(name), " ")
	d.LastSeen = time.Now()
	switch {
	case port == 50000 && b[10] == 0x06:
		if len(b) < 0x36 {
			return d, fmt.Errorf("prolink: truncated announcement")
		}
		d.Number = b[0x24]
	case port == 50001 && b[10] == 0x0b:
		if len(b) < 0x3c {
			return d, fmt.Errorf("prolink: truncated absolute position packet")
		}
		d.Number = b[0x21]
		trackLen := binary.BigEndian.Uint32(b[0x24:0x28])
		if trackLen > 0 && trackLen != 0xffffffff {
			d.DurationMS = trackLen * 1000
		}
		playhead := binary.BigEndian.Uint32(b[0x28:0x2c])
		if playhead != 0xffffffff {
			d.PlayPositionMS = playhead
			d.PrecisePosition = true
			d.lastPrecise = d.LastSeen
		}
		rawPitch := int32(binary.BigEndian.Uint32(b[0x2c:0x30]))
		d.Pitch = float64(rawPitch) / 100.0
		rawBPM := binary.BigEndian.Uint32(b[0x38:0x3c])
		if rawBPM > 0 && rawBPM != 0xffffffff {
			d.EffectiveBPM = float64(rawBPM) / 10.0
			if 1+d.Pitch/100 > 0 {
				d.BPM = d.EffectiveBPM / (1 + d.Pitch/100)
			}
		}
	case port == 50001 && b[10] == 0x28:
		if len(b) < 0x60 {
			return d, fmt.Errorf("prolink: truncated beat packet")
		}
		d.Number = b[0x21]
		rawPitch := binary.BigEndian.Uint32(b[0x54:0x58])
		d.Pitch = (float64(rawPitch) - float64(0x100000)) / float64(0x100000) * 100
		rawBPM := binary.BigEndian.Uint16(b[0x5a:0x5c])
		if rawBPM > 0 && rawBPM != 0xffff {
			d.BPM = float64(rawBPM) / 100.0
			d.EffectiveBPM = d.BPM * (1 + d.Pitch/100)
		}
		if b[0x5c] >= 1 && b[0x5c] <= 4 {
			d.BeatInBar = b[0x5c]
		}
		d.Playing = true
	case port == 50002 && b[10] == 0x0a:
		if len(b) < 0xcc {
			return d, fmt.Errorf("prolink: truncated player status")
		}
		d.Number = b[0x21]
		d.SourcePlayer = b[0x28]
		d.SourceSlot = b[0x29]
		d.TrackID = binary.BigEndian.Uint32(b[0x2c:])
		d.Playing = b[0x89]&0x40 != 0 || len(b) < 0xd4 && (b[0x7b] == 3 || b[0x7b] == 4 || b[0x7b] == 9)
		d.Master = b[0x89]&0x20 != 0
		d.Sync = b[0x89]&0x10 != 0
		d.OnAir = b[0x89]&0x08 != 0
		rawPitch := binary.BigEndian.Uint32(b[0x8c:0x90])
		d.Pitch = (float64(rawPitch) - float64(0x100000)) / float64(0x100000) * 100
		rawBPM := binary.BigEndian.Uint16(b[0x92:0x94])
		if rawBPM > 0 && rawBPM != 0xffff {
			d.BPM = float64(rawBPM) / 100.0
			d.EffectiveBPM = d.BPM * (1 + d.Pitch/100)
		}
		rawBeat := binary.BigEndian.Uint32(b[0xa0:0xa4])
		if rawBeat > 0 && rawBeat != 0xffffffff {
			d.Beat = rawBeat
		}
		if b[0xa6] >= 1 && b[0xa6] <= 4 {
			d.BeatInBar = b[0xa6]
		}
		// 0xb4 is not a timestamp (0xb7 is CDJ-3000 media presence).
		if d.Beat > 0 && d.BPM > 0 {
			d.PlayPositionMS = uint32(float64(d.Beat-1) * 60000.0 / d.BPM)
		}
	default:
		return d, fmt.Errorf("prolink: unsupported packet type/port")
	}
	return d, nil
}

// ExtractANLZ extracts waveform columns, beat points and duration from a Rekordbox Analysis.
func ExtractANLZ(a *rekordbox.Analysis) (waveform [][3]byte, beats []BeatGridPoint, durationMS uint32) {
	if a == nil {
		return nil, nil, 0
	}
	if len(a.Beats) > 0 {
		durationMS = a.Beats[len(a.Beats)-1].TimeMS
		n := len(a.Beats)
		if n > 512 {
			n = 512
		}
		beats = make([]BeatGridPoint, n)
		for i := 0; i < n; i++ {
			beats[i] = BeatGridPoint{
				Beat:   a.Beats[i].Number,
				BPM:    a.Beats[i].BPM,
				TimeMS: a.Beats[i].TimeMS,
			}
		}
	}

	var wave *rekordbox.Waveform
	pri := map[string]int{"PWV7": 5, "PWV5": 4, "PWV4": 3, "PWV3": 2, "PWAV": 2, "PWV2": 2}
	for i := range a.Waveforms {
		w := &a.Waveforms[i]
		if len(w.Data) > 0 && w.EntryBytes > 0 && (wave == nil || pri[w.Tag] > pri[wave.Tag]) {
			wave = w
		}
	}
	if wave != nil && int(wave.EntryBytes) <= len(wave.Data) {
		total := len(wave.Data) / int(wave.EntryBytes)
		getCol := func(i int) [3]byte {
			off := i * int(wave.EntryBytes)
			p := wave.Data[off : off+int(wave.EntryBytes)]
			switch wave.Tag {
			case "PWV7":
				if len(p) >= 3 {
					// 3-band [mid, high, low] -> [Low, Mid, High]
					return [3]byte{p[2], p[0], p[1]}
				}
			case "PWV5":
				if len(p) >= 2 {
					v := binary.BigEndian.Uint16(p)
					h := float64((v>>2)&31) / 31.0
					r := byte(float64((v>>13)&7) * 255.0 / 7.0 * h)
					g := byte(float64((v>>10)&7) * 255.0 / 7.0 * h)
					b := byte(float64((v>>7)&7) * 255.0 / 7.0 * h)
					return [3]byte{r, g, b}
				}
			case "PWV4":
				if len(p) >= 6 {
					return [3]byte{p[3], p[4], p[5]}
				}
			}
			v := byte(int(p[0]&31) * 255 / 31)
			return [3]byte{v, v, v}
		}
		if total <= 256 {
			waveform = make([][3]byte, total)
			for i := 0; i < total; i++ {
				waveform[i] = getCol(i)
			}
		} else {
			const bins = 256
			waveform = make([][3]byte, bins)
			for i := 0; i < bins; i++ {
				lo := i * total / bins
				hi := (i + 1) * total / bins
				if hi <= lo {
					hi = lo + 1
				}
				var peak [3]byte
				for j := lo; j < hi && j < total; j++ {
					col := getCol(j)
					if col[0] > peak[0] {
						peak[0] = col[0]
					}
					if col[1] > peak[1] {
						peak[1] = col[1]
					}
					if col[2] > peak[2] {
						peak[2] = col[2]
					}
				}
				waveform[i] = peak
			}
		}
	}
	return waveform, beats, durationMS
}

type Snapshot struct {
	UpdatedAt time.Time         `json:"updatedAt"`
	Devices   []Device          `json:"devices"`
	Decks     map[string]Device `json:"decks"`
}

// BuildSnapshot filters active devices and populates standard decks map.
func BuildSnapshot(devices []Device) Snapshot {
	now := time.Now()
	active := make([]Device, 0, len(devices))
	for _, d := range devices {
		if d.LastSeen.IsZero() || now.Sub(d.LastSeen) <= 10*time.Second {
			active = append(active, d)
		}
	}
	decks := make(map[string]Device)
	for _, d := range active {
		if d.Number >= 1 && d.Number <= 4 {
			decks[strconv.Itoa(int(d.Number))] = d
		}
	}
	if _, ok3 := decks["3"]; !ok3 {
		if d1, ok1 := decks["1"]; ok1 {
			decks["3"] = d1
		}
	}
	if _, ok4 := decks["4"]; !ok4 {
		if d2, ok2 := decks["2"]; ok2 {
			decks["4"] = d2
		}
	}
	return Snapshot{
		UpdatedAt: now,
		Devices:   active,
		Decks:     decks,
	}
}

const maxDevices = 64

type cachedTrack struct {
	analysis   *rekordbox.Analysis
	title      string
	artist     string
	waveform   [][3]byte
	beats      []BeatGridPoint
	durationMS uint32
}

type Discovery struct {
	mu         sync.RWMutex
	devices    map[string]Device
	anlzRoot   string
	resolver   func(Device) (*rekordbox.Analysis, string, string)
	trackCache map[string]cachedTrack
	trackKeys  []string
	media      *MediaCache
}

// SetMediaCache reads tracks from the hosting player over the network first (#59).
func (d *Discovery) SetMediaCache(m *MediaCache) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.media = m
}

// hostIP runs with the mutex held: the address of player number n.
func (d *Discovery) hostIP(n byte) net.IP {
	for _, dev := range d.devices {
		if dev.Number == n && dev.Address != "" {
			return net.ParseIP(dev.Address)
		}
	}
	return nil
}

func NewDiscovery() *Discovery {
	return &Discovery{
		devices:    map[string]Device{},
		trackCache: map[string]cachedTrack{},
	}
}

func (d *Discovery) SetANLZRoot(root string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.anlzRoot = root
}

func (d *Discovery) SetResolver(fn func(Device) (*rekordbox.Analysis, string, string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolver = fn
}

func defaultResolveWithRoot(anlzRoot string, device Device) (*rekordbox.Analysis, string, string) {
	if anlzRoot == "" || device.TrackID == 0 {
		return nil, "", ""
	}
	var title, artist, anlzRelPath string
	pdbPaths := []string{
		filepath.Join(anlzRoot, "PIONEER", "rekordbox", "export.pdb"),
		filepath.Join(anlzRoot, "rekordbox", "export.pdb"),
	}
	for _, pdbPath := range pdbPaths {
		if lib, err := rekordbox.ReadPDB(pdbPath); err == nil && lib != nil {
			for _, trk := range lib.Tracks {
				if trk.ID == device.TrackID {
					title = trk.Title
					artist = trk.Artist
					anlzRelPath = trk.AnalysisPath
					break
				}
			}
			if anlzRelPath != "" || title != "" {
				break
			}
		}
	}
	if anlzRelPath == "" {
		dbPaths := []string{
			filepath.Join(anlzRoot, "PIONEER", "rekordbox", "exportLibrary.db"),
			filepath.Join(anlzRoot, "rekordbox", "exportLibrary.db"),
		}
		for _, dbPath := range dbPaths {
			if lib, err := rekordbox.ReadSQLite(context.Background(), dbPath, nil); err == nil && lib != nil {
				for _, trk := range lib.Tracks {
					if trk.ID == device.TrackID {
						title = trk.Title
						artist = trk.Artist
						anlzRelPath = trk.AnalysisPath
						break
					}
				}
				if anlzRelPath != "" || title != "" {
					break
				}
			}
		}
	}

	var basePaths []string
	if anlzRelPath != "" {
		clean := strings.TrimPrefix(anlzRelPath, "/")
		clean = strings.TrimSuffix(clean, filepath.Ext(clean))
		basePaths = append(basePaths, filepath.Join(anlzRoot, clean))
	}

	var foundBases []string
	anlzDir := filepath.Join(anlzRoot, "PIONEER", "USBANLZ")
	if _, err := os.Stat(anlzDir); os.IsNotExist(err) {
		anlzDir = anlzRoot
	}
	_ = filepath.WalkDir(anlzDir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		name := strings.ToUpper(de.Name())
		if strings.HasPrefix(name, "ANLZ") && (strings.HasSuffix(name, ".DAT") || strings.HasSuffix(name, ".EXT") || strings.HasSuffix(name, ".2EX")) {
			base := path[:len(path)-len(filepath.Ext(path))]
			foundBases = append(foundBases, base)
		}
		return nil
	})

	trackIDHex := fmt.Sprintf("%04X", device.TrackID)
	trackIDDec := fmt.Sprintf("%04d", device.TrackID)
	trackIDDecMinus1 := fmt.Sprintf("%04d", device.TrackID-1)
	for _, b := range foundBases {
		ub := strings.ToUpper(b)
		if strings.Contains(ub, trackIDHex) || strings.Contains(ub, trackIDDec) || strings.Contains(ub, trackIDDecMinus1) {
			basePaths = append(basePaths, b)
		}
	}
	if len(basePaths) == 0 && len(foundBases) == 1 {
		basePaths = append(basePaths, foundBases[0])
	}

	seenBase := make(map[string]bool)
	var uniqueBases []string
	for _, b := range basePaths {
		if !seenBase[b] {
			seenBase[b] = true
			uniqueBases = append(uniqueBases, b)
		}
	}

	var merged *rekordbox.Analysis
	exts := []string{".DAT", ".EXT", ".2EX", ".dat", ".ext", ".2ex"}
	for _, base := range uniqueBases {
		for _, ext := range exts {
			filePath := base + ext
			if a, err := rekordbox.ReadAnalysis(filePath); err == nil && a != nil {
				if merged == nil {
					merged = a
				} else {
					merged.Merge(a)
				}
			}
		}
		if merged != nil {
			break
		}
	}

	return merged, title, artist
}

func (d *Discovery) defaultResolve(device Device) (*rekordbox.Analysis, string, string) {
	return defaultResolveWithRoot(d.anlzRoot, device)
}

func (d *Discovery) Observe(device Device) Device {
	d.mu.Lock()
	if d.devices == nil {
		d.devices = map[string]Device{}
	}
	now := time.Now()
	d.prune(now)
	if device.LastSeen.IsZero() {
		device.LastSeen = now
	}
	key := fmt.Sprintf("%s/%d", device.Address, device.Number)
	// Spoofed UDP must not grow the table without bound; a real network has a
	// handful of players and mixers (#36).
	if _, ok := d.devices[key]; !ok && len(d.devices) >= maxDevices {
		d.mu.Unlock()
		return device
	}
	if old, ok := d.devices[key]; ok {
		if device.PacketType == 0x28 {
			// Beat packets carry tempo/bar phase, not status flags or track position.
			device.Master, device.Sync, device.OnAir = old.Master, old.Sync, old.OnAir
			device.Beat, device.PlayPositionMS = old.Beat, old.PlayPositionMS
			device.PrecisePosition, device.lastPrecise = old.PrecisePosition, old.lastPrecise
		}
		if device.PacketType == 6 || device.PacketType == 0x28 || device.PacketType == 0x0b {
			if device.TrackID == 0 {
				device.TrackID = old.TrackID
			}
			if device.SourcePlayer == 0 {
				device.SourcePlayer = old.SourcePlayer
			}
			if device.SourceSlot == 0 {
				device.SourceSlot = old.SourceSlot
			}
			if len(device.Waveform) == 0 {
				device.Waveform = old.Waveform
			}
			if len(device.Beats) == 0 {
				device.Beats = old.Beats
			}
			if device.Title == "" {
				device.Title = old.Title
			}
			if device.Artist == "" {
				device.Artist = old.Artist
			}
			if device.DurationMS == 0 {
				device.DurationMS = old.DurationMS
			}
		}
		if device.PacketType == 0x0b {
			device.Master, device.Sync, device.OnAir = old.Master, old.Sync, old.OnAir
			if device.Beat == 0 {
				device.Beat = old.Beat
			}
			if device.BeatInBar == 0 {
				device.BeatInBar = old.BeatInBar
			}
			if !device.Playing {
				device.Playing = old.Playing
			}
			if !device.Sync {
				device.Sync = old.Sync
			}
			if old.BPM > 0 {
				device.BPM = old.BPM
			}
			if device.EffectiveBPM == 0 {
				device.EffectiveBPM = old.EffectiveBPM
			}
			if !device.PrecisePosition {
				device.PlayPositionMS = old.PlayPositionMS
				device.PrecisePosition = old.PrecisePosition
				device.lastPrecise = old.lastPrecise
			} else {
				device.lastPrecise = device.LastSeen
			}
		}
		if device.PacketType != 0x0b && old.PrecisePosition && now.Sub(old.lastPrecise) <= time.Second && (device.TrackID == 0 || old.TrackID == 0 || device.TrackID == old.TrackID) {
			device.PlayPositionMS = old.PlayPositionMS
			device.PrecisePosition = true
			device.lastPrecise = old.lastPrecise
		}
		if device.PacketType == 6 {
			if device.BPM == 0 {
				device.BPM = old.BPM
			}
			if device.Pitch == 0 {
				device.Pitch = old.Pitch
			}
			if device.EffectiveBPM == 0 {
				device.EffectiveBPM = old.EffectiveBPM
			}
			if device.Beat == 0 {
				device.Beat = old.Beat
			}
			if device.BeatInBar == 0 {
				device.BeatInBar = old.BeatInBar
			}
			if device.PlayPositionMS == 0 {
				device.PlayPositionMS = old.PlayPositionMS
			}
			if !device.Playing {
				device.Playing = old.Playing
			}
			if !device.OnAir {
				device.OnAir = old.OnAir
			}
			if !device.Master {
				device.Master = old.Master
			}
			if !device.Sync {
				device.Sync = old.Sync
			}
		}
	}

	var (
		needsResolve bool
		resolver     func(Device) (*rekordbox.Analysis, string, string)
		anlzRoot     string
		cacheKey     string
		media        *MediaCache
		host         net.IP
	)
	if device.TrackID != 0 && (len(device.Waveform) == 0 || len(device.Beats) == 0 || device.Title == "") {
		cacheKey = fmt.Sprintf("%d:%d:%d", device.SourcePlayer, device.SourceSlot, device.TrackID)
		if cached, ok := d.trackCache[cacheKey]; ok {
			if len(device.Waveform) == 0 {
				device.Waveform = cached.waveform
			}
			if len(device.Beats) == 0 {
				device.Beats = cached.beats
			}
			if device.DurationMS == 0 {
				device.DurationMS = cached.durationMS
			}
			if device.Title == "" {
				device.Title = cached.title
			}
			if device.Artist == "" {
				device.Artist = cached.artist
			}
		} else if d.resolver != nil || d.anlzRoot != "" || d.media != nil {
			needsResolve = true
			resolver = d.resolver
			anlzRoot = d.anlzRoot
			// Remote media (#59): look the host up now, fetch over NFS with the lock released.
			if media = d.media; media != nil {
				if host = d.hostIP(device.SourcePlayer); host == nil && device.SourcePlayer == device.Number {
					host = net.ParseIP(device.Address)
				}
			}
		}
	}

	if needsResolve {
		d.mu.Unlock()

		var anlz *rekordbox.Analysis
		var title, artist string
		if resolver != nil {
			anlz, title, artist = resolver(device)
		} else {
			if media != nil {
				anlz, title, artist = media.resolve(device, host)
			}
			if anlz == nil && title == "" && anlzRoot != "" {
				anlz, title, artist = defaultResolveWithRoot(anlzRoot, device)
			}
		}
		var wf [][3]byte
		var bts []BeatGridPoint
		var dur uint32
		if anlz != nil {
			wf, bts, dur = ExtractANLZ(anlz)
		}
		entry := cachedTrack{
			analysis:   anlz,
			title:      title,
			artist:     artist,
			waveform:   wf,
			beats:      bts,
			durationMS: dur,
		}

		d.mu.Lock()
		if d.trackCache == nil {
			d.trackCache = make(map[string]cachedTrack)
		}
		if _, exists := d.trackCache[cacheKey]; !exists {
			if len(d.trackCache) >= 64 && len(d.trackKeys) > 0 {
				evict := d.trackKeys[0]
				d.trackKeys = d.trackKeys[1:]
				delete(d.trackCache, evict)
			}
			d.trackCache[cacheKey] = entry
			d.trackKeys = append(d.trackKeys, cacheKey)
		}
		if len(device.Waveform) == 0 {
			device.Waveform = wf
		}
		if len(device.Beats) == 0 {
			device.Beats = bts
		}
		if device.DurationMS == 0 {
			device.DurationMS = dur
		}
		if device.Title == "" {
			device.Title = title
		}
		if device.Artist == "" {
			device.Artist = artist
		}
	}

	if !device.PrecisePosition && len(device.Beats) > 0 && device.Beat > 0 && int(device.Beat-1) < len(device.Beats) && device.PlayPositionMS == 0 {
		device.PlayPositionMS = device.Beats[device.Beat-1].TimeMS
	}

	d.devices[key] = device
	d.mu.Unlock()
	return device
}

func (d *Discovery) Devices(now time.Time) []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune(now)
	var out []Device
	for _, v := range d.devices {
		if now.Sub(v.LastSeen) <= 10*time.Second {
			out = append(out, v)
		}
	}
	return out
}

// prune runs while the discovery mutex is held.
func (d *Discovery) prune(now time.Time) {
	for key, device := range d.devices {
		if now.Sub(device.LastSeen) > 10*time.Second {
			delete(d.devices, key)
		}
	}
}

// Listen listens to discovery, beat, and status broadcasts on the selected interface
// address, with bounded reads and context cancellation. No network writes occur.
func (d *Discovery) Listen(ctx context.Context, address net.IP, notify func(Device)) error {
	var conns []*net.UDPConn
	host := ""
	if address != nil {
		host = address.String()
	}
	for _, port := range []int{50000, 50001, 50002} {
		c, e := listenLinkUDP(net.JoinHostPort(host, strconv.Itoa(port)), false)
		if e != nil {
			if port == 50001 {
				continue
			}
			for _, old := range conns {
				old.Close()
			}
			return e
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	errs := make(chan error, len(conns))
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			b := make([]byte, 2048)
			for {
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				n, addr, e := c.ReadFromUDP(b)
				if ctx.Err() != nil {
					return
				}
				if ne, ok := e.(net.Error); ok && ne.Timeout() {
					continue
				}
				if e != nil {
					errs <- e
					return
				}
				device, e := ParsePacket(b[:n], c.LocalAddr().(*net.UDPAddr).Port, addr.IP)
				if e != nil {
					continue
				}
				device = d.Observe(device)
				if notify != nil {
					notify(device)
				}
			}
		}(c)
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	}
	for _, c := range conns {
		c.Close()
	}
	wg.Wait()
	return err
}
