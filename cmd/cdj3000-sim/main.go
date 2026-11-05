// cdj3000-sim is a software stand-in for a Pioneer CDJ-3000 on Pro DJ Link,
// for testing EngineX OS's CDJ -> Denon support (#51, #59) without hardware.
// It plays the CDJ side (keepalive 50000, beat 50001, status 50002, NFSv2 /C/)
// and runs EngineX's own receiver (pkg/prolink Discovery, Stage, rb-bridge) on
// another loopback address, with a Web UI showing both sides and the wire.
//
// Packet layouts follow the protocol tests in pkg/prolink (Deep Symmetry's
// analysis), not a real-CDJ capture: passing here proves EngineX and this
// simulator agree, not that a real CDJ-3000 does.
//
//	cdj3000-sim -http 127.0.0.1:18300 [-usb <rekordbox USB root>]
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/bridge"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/enginedb"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/prolink"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/testfixture"
)

//go:embed index.html
var indexHTML []byte

type wireEntry struct {
	At   string `json:"at"`
	Dir  string `json:"dir"`
	Port int    `json:"port"`
	Kind string `json:"kind"`
	Len  int    `json:"len"`
	Hex  string `json:"hex"`
}

type track struct {
	ID         uint32                  `json:"id"`
	Title      string                  `json:"title"`
	Artist     string                  `json:"artist"`
	BPM        float64                 `json:"bpm"`
	Path       string                  `json:"path"`
	DurationMS uint32                  `json:"duration_ms"`
	Waveform   [][3]byte               `json:"-"`
	Beats      []prolink.BeatGridPoint `json:"-"`
}

type sim struct {
	usb, denonDrive, cache string
	simIP, denonIP         net.IP
	nfs                    *prolink.NFSServer
	discovery              *prolink.Discovery
	tracks                 []track
	denonEmpty             bool // Denon drive without its own rekordbox export

	mu       sync.Mutex
	number   byte
	playing  bool
	pitch    float64 // percent
	pitchMax float64 // ±6/10/16/100 (WIDE)
	cur      int     // index into tracks
	posMS    float64
	lastTick time.Time
	lastBeat uint32
	wire     []wireEntry
	staged   map[string]any
	keepNow  bool // announce a new player number at once

	exporter    *prolink.Exporter // #63 reverse export, Denon side
	denonPlayer byte              // player number the Denon export announces
	yields      []string
	linkName    string
	linkAudio   []byte
}

func main() {
	httpAddr := flag.String("http", "127.0.0.1:18300", "Web UI address (keep it on loopback; tunnel over SSH)")
	usb := flag.String("usb", "", "rekordbox USB root with PIONEER/rekordbox/export.pdb (default: synthetic test export)")
	simIP := flag.String("sim-ip", "127.0.0.2", "address the simulated CDJ-3000 sends from and serves NFS on")
	denonIP := flag.String("denon-ip", "127.0.0.1", "address EngineX's Pro DJ Link receiver listens on")
	denonEmpty := flag.Bool("denon-empty", false, "Denon drive holds no rekordbox export of its own")
	flag.Parse()
	s, err := newSim(*usb, net.ParseIP(*simIP).To4(), net.ParseIP(*denonIP).To4())
	if err != nil {
		log.Fatal(err)
	}
	s.denonEmpty = *denonEmpty
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("cdj3000-sim: %d tracks from %s; UI http://%s", len(s.tracks), s.usb, *httpAddr)
	log.Fatal(http.ListenAndServe(*httpAddr, s.routes()))
}

// writeFixture is testfixture.Export without testing.TB, with track 1 replaced
// by a 64-second drum track so the waveform has something to show.
func writeFixture(root string) error {
	base := "PIONEER/USBANLZ/P001/ANLZ0000"
	wav, dat, ext, ex2 := drumTrack()
	files := map[string][]byte{
		"PIONEER/rekordbox/export.pdb": testfixture.PDB(),
		base + ".DAT":                  dat,
		base + ".EXT":                  ext,
		base + ".2EX":                  ex2,
		"MUSIC/" + norm.NFD.String(testfixture.UnicodeName): wav,
		"MUSIC/two.flac":  testfixture.FLAC(),
		"MUSIC/three.wav": testfixture.WAV(false),
		"MUSIC/four.wav":  testfixture.WAV(false),
		"MUSIC/five.wav":  testfixture.WAV(false),
	}
	for rel, b := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func newSim(usb string, simIP, denonIP net.IP) (*sim, error) {
	if simIP == nil || denonIP == nil {
		return nil, fmt.Errorf("sim-ip and denon-ip must be IPv4")
	}
	work, err := os.MkdirTemp("", "cdj3000-sim-")
	if err != nil {
		return nil, err
	}
	if usb == "" {
		usb = filepath.Join(work, "usb")
		if err := writeFixture(usb); err != nil {
			return nil, err
		}
	}
	lib, err := rekordbox.ReadPDB(filepath.Join(usb, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		return nil, fmt.Errorf("usb: %w", err)
	}
	s := &sim{usb: usb, denonDrive: filepath.Join(work, "denon"), cache: filepath.Join(work, "cache"),
		simIP: simIP, denonIP: denonIP, number: 3, pitchMax: 10}
	for _, t := range lib.Tracks {
		tr := track{ID: t.ID, Title: t.Title, Artist: t.Artist, BPM: t.BPM, Path: t.Path, DurationMS: uint32(t.Duration * 1000)}
		if t.AnalysisPath != "" {
			// Prefer the colour waveform files, as a CDJ-3000 does.
			stem := filepath.Join(usb, filepath.FromSlash(t.AnalysisPath[:len(t.AnalysisPath)-4]))
			// Waveform and beatgrid live in different files; take each from the first that has it.
			for _, ext := range []string{".2EX", ".EXT", ".DAT"} {
				a, err := rekordbox.ReadAnalysis(stem + ext)
				if err != nil {
					continue
				}
				w, b, d := prolink.ExtractANLZ(a)
				if full := pwv7(a); len(full) > 0 {
					w = full
				}
				if len(tr.Waveform) == 0 {
					tr.Waveform = w
				}
				if len(tr.Beats) == 0 && len(b) > 0 {
					tr.Beats, tr.DurationMS = b, d
				}
			}
		}
		if tr.DurationMS == 0 {
			tr.DurationMS = 180000
		}
		s.tracks = append(s.tracks, tr)
	}
	if len(s.tracks) == 0 {
		return nil, fmt.Errorf("usb: export.pdb has no tracks")
	}
	return s, nil
}

// pwv7 is the whole 3-band waveform as [low, mid, high], up to 2400 columns;
// prolink.ExtractANLZ keeps 256, which is what EngineX itself displays.
func pwv7(a *rekordbox.Analysis) [][3]byte {
	for _, w := range a.Waveforms {
		if w.Tag != "PWV7" || w.EntryBytes != 3 {
			continue
		}
		n := len(w.Data) / 3
		cols := make([][3]byte, min(n, 2400))
		for i := range n {
			c := &cols[i*len(cols)/n]
			p := w.Data[3*i:]
			c[0], c[1], c[2] = max(c[0], p[2]), max(c[1], p[0]), max(c[2], p[1])
		}
		return cols
	}
	return nil
}

func (s *sim) start(ctx context.Context) error {
	s.nfs = &prolink.NFSServer{Root: s.usb}
	if err := s.nfs.Listen(net.JoinHostPort(s.simIP.String(), "0")); err != nil {
		return fmt.Errorf("nfs: %w", err)
	}
	if err := os.MkdirAll(s.denonDrive, 0o755); err != nil {
		return err
	}
	if !s.denonEmpty {
		if err := writeFixture(s.denonDrive); err != nil {
			return err
		}
	}
	s.discovery = prolink.NewDiscovery()
	s.discovery.SetMediaCache(&prolink.MediaCache{Dir: s.cache, Portmap: s.nfs.PortmapPort()})
	listening := make(chan error, 1)
	go func() {
		listening <- s.discovery.Listen(ctx, s.denonIP, func(d prolink.Device) {
			s.log("Denon parsed", 0, fmt.Sprintf("type 0x%02x player %d %s", d.PacketType, d.Number, d.Title), nil)
		})
	}()
	select {
	case err := <-listening:
		return fmt.Errorf("denon receiver: %w", err)
	case <-time.After(200 * time.Millisecond):
	}
	for _, port := range []int{50000, 50002} {
		if err := s.listenDenon(ctx, port); err != nil {
			return fmt.Errorf("cdj listener: %w", err)
		}
	}
	if s.denonEmpty {
		s.exporter = prolink.NewExporter(prolink.ExportConfig{}) // nothing to export
	} else if err := s.startExport(); err != nil {
		return fmt.Errorf("denon export: %w", err)
	}
	context.AfterFunc(ctx, func() { s.exporter.Close(); s.nfs.Close() })
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: s.simIP})
	if err != nil {
		return err
	}
	go s.run(ctx, conn)
	return nil
}

func (s *sim) log(dir string, port int, kind string, b []byte) {
	n := len(b)
	if len(b) > 64 {
		b = b[:64]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire = append(s.wire, wireEntry{At: time.Now().Format("15:04:05.000"), Dir: dir, Port: port, Kind: kind, Len: n, Hex: hex.EncodeToString(b)})
	if len(s.wire) > 400 {
		s.wire = s.wire[len(s.wire)-400:]
	}
}

func (s *sim) run(ctx context.Context, conn *net.UDPConn) {
	defer conn.Close()
	send := func(port int, kind string, b []byte) {
		if _, err := conn.WriteToUDP(b, &net.UDPAddr{IP: s.denonIP, Port: port}); err == nil {
			s.log("CDJ → Denon", port, kind, b)
		}
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var lastKeep, lastStatus time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			s.advance(now)
			beat, bib := s.beatAt()
			n, keep := s.number, s.keepNow || now.Sub(lastKeep) >= 1500*time.Millisecond
			s.keepNow = false
			status := now.Sub(lastStatus) >= 200*time.Millisecond
			var beatPkt, statusPkt []byte
			if s.playing && beat != s.lastBeat {
				beatPkt = s.beatPacket(bib)
			}
			s.lastBeat = beat
			if status {
				statusPkt = s.statusPacket(beat, bib)
			}
			s.mu.Unlock()
			if keep {
				send(50000, "keepalive 0x06", keepalivePacket(n, s.simIP))
				// The Denon export listens on its own port here, 50000 on a real network.
				if st := s.exporter.Status(); st.Enabled {
					if a, err := net.ResolveUDPAddr("udp4", st.DiscoveryAddress); err == nil {
						_, _ = conn.WriteToUDP(keepalivePacket(n, s.simIP), a)
					}
				}
				lastKeep = now
			}
			if beatPkt != nil {
				send(50001, "beat 0x28", beatPkt)
			}
			if statusPkt != nil {
				send(50002, "CDJ-3000 status 0x0a", statusPkt)
				lastStatus = now
			}
		}
	}
}

// advance moves the play head at the pitched rate and loops at the end.
func (s *sim) advance(now time.Time) {
	if !s.lastTick.IsZero() && s.playing {
		s.posMS += float64(now.Sub(s.lastTick).Milliseconds()) * (1 + s.pitch/100)
		if d := float64(s.tracks[s.cur].DurationMS); s.posMS >= d {
			s.posMS = math.Mod(s.posMS, d)
		}
	}
	s.lastTick = now
}

// beatAt is the beat number (1-based) and beat within bar at the play head.
func (s *sim) beatAt() (uint32, byte) {
	t := s.tracks[s.cur]
	if len(t.Beats) > 0 {
		i := 0
		for i+1 < len(t.Beats) && float64(t.Beats[i+1].TimeMS) <= s.posMS {
			i++
		}
		return uint32(i + 1), byte(t.Beats[i].Beat)
	}
	beat := uint32(s.posMS/(60000/s.bpm())) + 1
	return beat, byte((beat-1)%4 + 1)
}

func (s *sim) bpm() float64 {
	if b := s.tracks[s.cur].BPM; b > 0 {
		return b
	}
	return 120
}

func pitchWord(pct float64) uint32 { return uint32(math.Round(0x100000 * (1 + pct/100))) }

func header(kind byte, size, nameAt int, number byte) []byte {
	b := make([]byte, size)
	copy(b, "Qspt1WmJOL")
	b[10] = kind
	copy(b[nameAt:nameAt+20], "CDJ-3000")
	b[nameAt+20] = 1
	if nameAt == 12 {
		b[0x21] = 2
		binary.BigEndian.PutUint16(b[0x22:], uint16(size))
		b[0x24] = number
	} else {
		b[0x21] = number
		binary.BigEndian.PutUint16(b[0x22:], uint16(size-0x24))
	}
	return b
}

func keepalivePacket(number byte, ip net.IP) []byte {
	b := header(6, 54, 12, number)
	b[0x25] = 1
	copy(b[0x26:], []byte{2, 0, 0, 0, 0, 0x20}) // fixed: a real CDJ keeps its MAC when its number changes
	copy(b[0x2c:], ip.To4())
	b[0x30] = 2
	b[0x34] = 1
	return b
}

func (s *sim) statusPacket(beat uint32, bib byte) []byte {
	n := s.number
	b := header(0x0a, 0x200, 11, n)
	b[0x20] = 6
	b[0x24] = n
	b[0x26], b[0x27] = 1, 1
	b[0x28], b[0x29], b[0x2a] = n, 3, 1 // track from this player's USB, rekordbox
	binary.BigEndian.PutUint32(b[0x2c:], s.tracks[s.cur].ID)
	b[0x6a], b[0x6b], b[0x73], b[0x75] = 4, 4, 4, 1
	b[0x7b] = 5 // P1 paused
	b[0x89] = 0xb8
	b[0x8b] = 0xfe
	if s.playing {
		b[0x7b], b[0x89], b[0x8b] = 3, 0xf8, 0xfa
	}
	copy(b[0x7c:], "3.20")
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		binary.BigEndian.PutUint32(b[off:], pitchWord(s.pitch))
	}
	binary.BigEndian.PutUint16(b[0x90:], 0x8000)
	binary.BigEndian.PutUint16(b[0x92:], uint16(math.Round(s.bpm()*100)))
	b[0x9f] = 0xff
	binary.BigEndian.PutUint32(b[0xa0:], beat)
	b[0xa6] = bib
	b[0xb7] = 1
	return b
}

func (s *sim) beatPacket(bib byte) []byte {
	b := header(0x28, 0x60, 11, s.number)
	ms := 60000 / (s.bpm() * (1 + s.pitch/100))
	for i, beats := range []float64{1, 2, float64(5 - bib), 4, float64(9 - bib), 8} {
		binary.BigEndian.PutUint32(b[0x24+4*i:], uint32(beats*ms))
	}
	binary.BigEndian.PutUint32(b[0x54:], pitchWord(s.pitch))
	binary.BigEndian.PutUint16(b[0x5a:], uint16(math.Round(s.bpm()*100)))
	b[0x5c] = bib
	return b
}

// stage is #59 end to end: copy the CDJ's USB over NFS onto the Denon drive,
// then let rb-bridge build the Engine library with the [Pro DJ Link] folder.
func (s *sim) stage(ctx context.Context) (map[string]any, error) {
	client := &prolink.NFSClient{IP: s.simIP, Portmap: s.nfs.PortmapPort()}
	started := time.Now()
	count, err := prolink.Stage(ctx, client, "/C/", filepath.Join(s.denonDrive, prolink.StagedDir, "CDJ-3000"), 1<<30)
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	s.log("Denon ← CDJ", 0, fmt.Sprintf("NFSv2 /C/: %d files staged", count), nil)
	res, err := bridge.ConvertAtMount(ctx, s.denonDrive, enginedb.Options{}, true)
	if err != nil {
		return nil, fmt.Errorf("rb-bridge: %w", err)
	}
	if res.Bypassed {
		return nil, fmt.Errorf("rb-bridge skipped the drive (already up to date or no export found)")
	}
	db, err := sql.Open("sqlite", filepath.Join(s.denonDrive, "Engine Library", "Database2", "m.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT p.title, (SELECT count(*) FROM PlaylistEntity e WHERE e.listId = p.id)
		FROM Playlist p ORDER BY p.parentListId, p.title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type playlist struct {
		Title  string `json:"title"`
		Tracks int    `json:"tracks"`
	}
	var lists []playlist
	for rows.Next() {
		var p playlist
		if err := rows.Scan(&p.Title, &p.Tracks); err != nil {
			return nil, err
		}
		lists = append(lists, p)
	}
	out := map[string]any{"files": count, "added": res.Added, "playlists": lists, "ms": time.Since(started).Milliseconds()}
	s.mu.Lock()
	s.staged = out
	s.mu.Unlock()
	return out, rows.Err()
}

func strip(devs []prolink.Device) []map[string]any {
	out := []map[string]any{}
	for _, d := range devs {
		b, _ := json.Marshal(d)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		delete(m, "waveform")
		delete(m, "beats")
		m["waveform_columns"], m["beat_points"] = len(d.Waveform), len(d.Beats)
		out = append(out, m)
	}
	return out
}

func (s *sim) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		snap := prolink.BuildSnapshot(s.discovery.Devices(time.Now()))
		s.mu.Lock()
		beat, bib := s.beatAt()
		wire := append([]wireEntry(nil), s.wire[max(0, len(s.wire)-120):]...)
		state := map[string]any{
			"cdj": map[string]any{"number": s.number, "playing": s.playing, "pitch": s.pitch, "pitch_max": s.pitchMax,
				"track": s.tracks[s.cur], "position_ms": s.posMS, "beat": beat, "beat_in_bar": bib, "bpm": s.bpm(),
				"effective_bpm": s.bpm() * (1 + s.pitch/100), "ip": s.simIP.String(), "portmap": s.nfs.PortmapPort()},
			"tracks": s.tracks, "denon": strip(snap.Devices), "staged": s.staged, "wire": wire,
			"export": s.exporter.Status(), "denon_player": s.denonPlayer, "yields": s.yields[max(0, len(s.yields)-8):],
		}
		s.mu.Unlock()
		writeJSON(w, state)
	})
	mux.HandleFunc("GET /api/waveform", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if r.URL.Query().Get("side") == "denon" {
			for _, d := range s.discovery.Devices(time.Now()) {
				if d.TrackID == uint32(id) {
					writeJSON(w, map[string]any{"waveform": d.Waveform, "beats": d.Beats, "duration_ms": d.DurationMS})
					return
				}
			}
			writeJSON(w, map[string]any{})
			return
		}
		for _, t := range s.tracks {
			if t.ID == uint32(id) {
				writeJSON(w, map[string]any{"waveform": t.Waveform, "beats": t.Beats, "duration_ms": t.DurationMS})
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /audio", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		for _, t := range s.tracks {
			// Only paths listed in export.pdb, resolved inside the USB root.
			if rel, ok := prolink.SafeRel(t.Path); t.ID == uint32(id) && ok {
				http.ServeFile(w, r, filepath.Join(s.usb, rel))
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /api/control", func(w http.ResponseWriter, r *http.Request) {
		var c struct {
			Number   *byte    `json:"number"`
			Playing  *bool    `json:"playing"`
			Pitch    *float64 `json:"pitch"`
			PitchMax *float64 `json:"pitch_max"`
			Track    *uint32  `json:"track"`
			Cue      bool     `json:"cue"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if c.Number != nil && *c.Number >= 1 && *c.Number <= 4 && *c.Number != s.number {
			s.number, s.keepNow = *c.Number, true
		}
		if c.Playing != nil {
			s.playing = *c.Playing
		}
		if c.PitchMax != nil {
			switch *c.PitchMax {
			case 6, 10, 16, 100:
				s.pitchMax = *c.PitchMax
			}
		}
		if c.Pitch != nil {
			s.pitch = *c.Pitch
		}
		s.pitch = math.Max(-s.pitchMax, math.Min(s.pitchMax, s.pitch))
		if c.Track != nil {
			for i, t := range s.tracks {
				if t.ID == *c.Track {
					s.cur, s.posMS = i, 0
				}
			}
		}
		if c.Cue {
			s.posMS, s.playing = 0, false
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.linkRoutes(mux)
	mux.HandleFunc("POST /api/stage", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.stage(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
