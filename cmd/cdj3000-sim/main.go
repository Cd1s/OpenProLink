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
	"bytes"
	"context"
	"database/sql"
	_ "embed"
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
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/Cd1s/OpenCDJ-3000/pkg/cdj"
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
	Remote     bool                    `json:"remote,omitempty"`    // loaded from the Denon over LINK
	RemoteID   uint32                  `json:"remote_id,omitempty"` // its rekordbox ID on the Denon's drive
	Waveform   [][3]byte               `json:"-"`
	Beats      []prolink.BeatGridPoint `json:"-"`
}

// remoteTrack marks the ID of a track loaded over LINK, so it never collides with ours.
const remoteTrack = 1 << 31

type sim struct {
	usb, denonDrive, cache string
	simIP, denonIP         net.IP
	deck                   net.IP   // a real deck on the LAN: packets go there, NFS answers on portmap 111
	decks                  []net.IP // every -deck; deck is the first. Each gets the CDJ's packets
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
	master   bool
	syncOn   bool
	nudge    float64 // percent, held pitch bend: moves the play head only, not the reported tempo
	cueMS    float64
	hotCues  map[uint32]map[string]float64 // track ID -> pad A-H -> position

	masterBPM  float64 // effective BPM of another player that claims master (BEAT SYNC follows it)
	masterSeen time.Time
	peerMaster map[byte]bool // master flag in each other player's last status
	// BEAT SYNC phase source: the master was masterBar beats into its bar (or beat,
	// without barKnown) at masterAt. Fed by beat packets, Ableton Link or /peer/decks.
	masterBar float64
	masterAt  time.Time
	barKnown  bool
	syncSnap  bool // BEAT SYNC just pressed: jump onto the master's beat once

	link         *ableLink         // Ableton Link peer, nil unless -ableton-link
	exporter     *prolink.Exporter // #63 reverse export, Denon side
	denonPlayer  byte              // player number the Denon export announces
	yields       []string
	linkName     string
	linkAudio    []byte // nil while it downloads
	linkAudioErr string
	linkLib      *rekordbox.Library // the Denon's export.pdb, parsed
	linkPDB      prolink.NFSFile    // its size and mtime when parsed
}

func main() {
	httpAddr := flag.String("http", "127.0.0.1:18300", "Web UI address (keep it on loopback; tunnel over SSH)")
	usb := flag.String("usb", "", "rekordbox USB root with PIONEER/rekordbox/export.pdb (default: synthetic test export)")
	simIP := flag.String("sim-ip", "127.0.0.2", "address the simulated CDJ-3000 sends from and serves NFS on")
	denonIP := flag.String("denon-ip", "127.0.0.1", "address EngineX's Pro DJ Link receiver listens on")
	denonEmpty := flag.Bool("denon-empty", false, "Denon drive holds no rekordbox export of its own")
	abl := flag.Bool("ableton-link", false, "publish tempo and bar phase as an Ableton Link session on -sim-ip's LAN")
	deck := flag.String("deck", "", "IPv4 of a real deck on the LAN to play to, or several separated by commas; the first hosts export and master handover (needs -sim-ip set to this host's LAN address)")
	flag.Parse()
	s, err := newSim(*usb, net.ParseIP(*simIP).To4(), net.ParseIP(*denonIP).To4())
	if err != nil {
		log.Fatal(err)
	}
	if *deck != "" {
		for _, a := range strings.Split(*deck, ",") {
			ip := net.ParseIP(strings.TrimSpace(a)).To4()
			if ip == nil {
				log.Fatal("-deck must be IPv4")
			}
			s.decks = append(s.decks, ip)
		}
		s.deck = s.decks[0]
	}
	s.denonEmpty = *denonEmpty
	s.playing, s.master = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		log.Fatal(err)
	}
	if *abl {
		if s.link, err = s.startAbletonLink(s.simIP); err != nil {
			log.Fatal(err)
		}
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
		simIP: simIP, denonIP: denonIP, number: 3, pitchMax: 10, hotCues: map[uint32]map[string]float64{}, peerMaster: map[byte]bool{}}
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

// pwv7 is the whole 3-band waveform as [low, mid, high] at full resolution
// (150 columns a second in rekordbox exports), as the CDJ-3000 scrolls it;
// prolink.ExtractANLZ keeps 256, which is what EngineX itself displays.
func pwv7(a *rekordbox.Analysis) [][3]byte {
	for _, w := range a.Waveforms {
		if w.Tag != "PWV7" || w.EntryBytes != 3 {
			continue
		}
		n := len(w.Data) / 3
		cols := make([][3]byte, n)
		for i := range cols {
			p := w.Data[3*i:]
			cols[i] = [3]byte{p[2], p[0], p[1]}
		}
		return cols
	}
	return nil
}

func (s *sim) start(ctx context.Context) error {
	s.nfs = &prolink.NFSServer{Root: s.usb}
	host, portmap := s.simIP.String(), "0"
	if s.deck != nil {
		// A real player asks the standard portmapper port; macOS lets a non-root user bind 111 only on the wildcard address.
		host, portmap = "0.0.0.0", "111"
	}
	if err := s.nfs.Listen(net.JoinHostPort(host, portmap)); err != nil {
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
	var lastParsedPosLog time.Time
	go func() {
		listening <- s.discovery.Listen(ctx, s.denonIP, func(d prolink.Device) {
			if d.PacketType == cdj.TypeAbsolutePosition {
				now := time.Now()
				if !lastParsedPosLog.IsZero() && now.Sub(lastParsedPosLog) < time.Second {
					return
				}
				lastParsedPosLog = now
			}
			s.log("Denon parsed", 0, fmt.Sprintf("type 0x%02x player %d %s", d.PacketType, d.Number, d.Title), nil)
		})
	}()
	select {
	case err := <-listening:
		return fmt.Errorf("denon receiver: %w", err)
	case <-time.After(200 * time.Millisecond):
	}
	for _, port := range []int{50000, 50001, 50002} {
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
	if s.deck != nil {
		go s.watchDeckLead(ctx)
	}
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

// targets are every real deck (other players only see a CDJ that talks to them),
// else the in-process receiver.
func (s *sim) targets() []net.IP {
	if len(s.decks) > 0 {
		return s.decks
	}
	return []net.IP{s.target()}
}

// isDeck reports whether ip is one of the -deck decks.
func (s *sim) isDeck(ip net.IP) bool {
	if s.deck != nil && s.deck.Equal(ip) {
		return true
	}
	for _, d := range s.decks {
		if d.Equal(ip) {
			return true
		}
	}
	return false
}

// target is where the virtual CDJ's packets go: the real deck, or the in-process receiver.
func (s *sim) target() net.IP {
	if s.deck != nil {
		return s.deck
	}
	return s.denonIP
}

func (s *sim) run(ctx context.Context, conn *net.UDPConn) {
	defer conn.Close()
	send := func(port int, kind string, b []byte) {
		sent := false
		for _, ip := range s.targets() {
			_, err := conn.WriteToUDP(b, &net.UDPAddr{IP: ip, Port: port})
			sent = sent || err == nil
		}
		if sent {
			s.log("CDJ → Denon", port, kind, b)
		}
	}
	tick := time.NewTicker(5 * time.Millisecond) // beat packets leave within 5 ms of the beat
	defer tick.Stop()
	var lastKeep, lastStatus, lastPos, lastPosLog time.Time
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
			pos := now.Sub(lastPos) >= 30*time.Millisecond
			var beatPkt, statusPkt, posPkt []byte
			if s.playing && beat != s.lastBeat {
				beatPkt = s.beatPacket(bib)
			}
			s.lastBeat = beat
			if status {
				statusPkt = s.statusPacket(beat, bib)
			}
			if pos && len(s.tracks) > 0 {
				durSec := uint32(s.tracks[s.cur].DurationMS / 1000)
				posPkt = cdj.NewAbsolutePositionPacket(cdj.AbsolutePosition{
					DeviceName:     "CDJ-3000",
					PlayerNumber:   n,
					TrackLengthSec: durSec,
					PlayheadMS:     uint32(math.Round(s.posMS)),
					HasPlayhead:    true,
					Pitch:          s.pitch,
					EffectiveBPM:   s.bpm() * (1 + s.pitch/100),
				})
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
			if posPkt != nil {
				sent := false
				for _, ip := range s.targets() {
					_, err := conn.WriteToUDP(posPkt, &net.UDPAddr{IP: ip, Port: 50001})
					sent = sent || err == nil
				}
				if sent {
					if lastPosLog.IsZero() || now.Sub(lastPosLog) >= time.Second {
						s.log("CDJ → Denon", 50001, "absolute pos 0x0b", posPkt)
						lastPosLog = now
					}
				}
				lastPos = now
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

// advance moves the play head at the pitched (and nudged) rate and loops at
// the end. BEAT SYNC (without MASTER) follows the master's tempo and, when a
// phase source is live, its beat or bar phase.
func (s *sim) advance(now time.Time) {
	follow := s.syncOn && !s.master
	if follow && s.masterBPM > 0 && now.Sub(s.masterSeen) < 2*time.Second {
		need := (s.masterBPM/s.bpm() - 1) * 100
		for _, r := range []float64{6, 10, 16, 100} { // widen the tempo range as a CDJ does for sync
			if math.Abs(need) <= s.pitchMax {
				break
			}
			s.pitchMax = math.Max(s.pitchMax, r)
		}
		s.pitch = math.Max(-s.pitchMax, math.Min(s.pitchMax, need))
	}
	if !s.lastTick.IsZero() && s.playing {
		s.posMS += float64(now.Sub(s.lastTick).Microseconds()) / 1000 * (1 + (s.pitch+s.nudge)/100)
		if d := float64(s.tracks[s.cur].DurationMS); s.posMS >= d {
			s.posMS = math.Mod(s.posMS, d)
		}
	}
	s.lastTick = now
	if follow && s.playing && s.masterBPM > 0 && now.Sub(s.masterAt) < 2*time.Second {
		s.lockPhase(now)
	}
}

// lockPhase moves the play head onto the master's beat (its bar, when the source
// says where the bar starts): a jump when BEAT SYNC is pressed or the error passes
// 25 ms, a quarter of the error per tick for drift above 2 ms.
func (s *sim) lockPhase(now time.Time) {
	period := 1.0
	if s.barKnown {
		period = 4
	}
	target := s.masterBar + now.Sub(s.masterAt).Seconds()*s.masterBPM/60
	d := target - s.barPhase(s.posMS)
	d -= period * math.Floor(d/period+0.5) // shortest way: [-period/2, period/2)
	errMS := d * 60000 / s.bpm()           // beats of this track, in track time
	switch {
	case s.syncSnap || math.Abs(errMS) > 25:
		s.posMS, s.syncSnap = math.Max(0, s.posMS+errMS), false
	case math.Abs(errMS) > 2:
		s.posMS += errMS / 4
	}
}

// sawMasterBeat records the master's position: bar phase (beats into the bar, or
// into the beat if !bar) at time at. Caller holds s.mu.
func (s *sim) sawMasterBeat(bpm, phase float64, bar bool, at time.Time) {
	s.masterBPM, s.masterSeen = bpm, at
	s.masterBar, s.masterAt, s.barKnown = phase, at, bar
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

func pitchWord(pct float64) uint32 { return cdj.PitchToWord(pct) }

func keepalivePacket(number byte, ip net.IP) []byte {
	return cdj.NewKeepalivePacket(cdj.Keepalive{
		DeviceName:      "CDJ-3000",
		PlayerNumber:    number,
		FirstDeviceFlag: 1,
		MAC:             net.HardwareAddr{2, 0, 0, 0, 0, 0x20}, // fixed: a real CDJ keeps its MAC when its number changes
		IP:              ip,
		PeerCount:       2,
		DeviceClass:     1,
		ModelCapability: 0,
	})
}

func (s *sim) statusPacket(beat uint32, bib byte) []byte {
	src, id := s.number, s.tracks[s.cur].ID
	if t := s.tracks[s.cur]; t.Remote {
		src, id = s.denonPlayer, t.RemoteID // loaded from the Denon's USB, as a CDJ reports a linked track
	}
	return cdj.NewCDJ3000StatusPacket(cdj.Status{
		DeviceName:        "CDJ-3000",
		PlayerNumber:      s.number,
		PacketVersion:     6,
		Active:            s.playing,
		TrackSourcePlayer: src,
		TrackSourceSlot:   cdj.SlotUSB,
		TrackType:         cdj.TrackTypeRekordbox,
		TrackID:           id,
		USBPresent:        true,
		USBLocalState:     0,
		SDLocalState:      4,
		LinkAvailable:     true,
		PlayState1:        cdj.PlayState1Paused,
		Firmware:          "3.20",
		Playing:           s.playing,
		Master:            s.master,
		Sync:              s.syncOn,
		OnAir:             true,
		PlayState2:        0xfe,
		Pitch:             s.pitch,
		BPM:               s.bpm(),
		Beat:              beat,
		BeatWithinBar:     bib,
		MediaPresence:     1,
		WaveformColor:     4,
	})
}

func (s *sim) beatPacket(bib byte) []byte {
	ms := 60000 / (s.bpm() * (1 + s.pitch/100))
	return cdj.NewBeatPacket(cdj.Beat{
		DeviceName:    "CDJ-3000",
		PlayerNumber:  s.number,
		NextBeatMS:    uint32(1 * ms),
		SecondBeatMS:  uint32(2 * ms),
		NextBarMS:     uint32(float64(5-bib) * ms),
		FourthBeatMS:  uint32(4 * ms),
		SecondBarMS:   uint32(float64(9-bib) * ms),
		EighthBeatMS:  uint32(8 * ms),
		Pitch:         s.pitch,
		BPM:           s.bpm(),
		BeatWithinBar: bib,
	})
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
		var abl map[string]any
		if s.link != nil {
			abl = s.link.status()
		}
		s.mu.Lock()
		wire := append([]wireEntry(nil), s.wire[max(0, len(s.wire)-120):]...)
		state := map[string]any{
			"cdj":    s.deckState(true),
			"tracks": append([]track(nil), s.tracks...), "denon": strip(snap.Devices), "ableton_link": abl, "deck": s.deck, "staged": s.staged, "wire": wire,
			"export": s.exporter.Status(), "denon_player": s.denonPlayer, "yields": s.yields[max(0, len(s.yields)-8):],
			"link_audio": map[string]any{"bytes": len(s.linkAudio), "error": s.linkAudioErr},
		}
		s.mu.Unlock()
		writeJSON(w, state)
	})
	// The UI polls this 10 times a second; /api/state (wire log, track list) once a second.
	mux.HandleFunc("GET /api/tick", func(w http.ResponseWriter, r *http.Request) {
		var abl map[string]any
		if s.link != nil {
			abl = s.link.status()
		}
		s.mu.Lock()
		state := map[string]any{"cdj": s.deckState(false), "ableton_link": abl, "link_audio": map[string]any{"bytes": len(s.linkAudio), "error": s.linkAudioErr}}
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
		if t, ok := s.track(uint32(id)); ok {
			writeJSON(w, map[string]any{"waveform": t.Waveform, "beats": t.Beats, "duration_ms": t.DurationMS})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /audio", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseUint(r.URL.Query().Get("id"), 10, 32)
		t, ok := s.track(uint32(id))
		if ok && t.Remote {
			s.mu.Lock()
			name, data := s.linkName, s.linkAudio
			s.mu.Unlock()
			if data == nil {
				http.Error(w, "still downloading over NFS", http.StatusServiceUnavailable)
				return
			}
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
			return
		}
		// Only paths listed in export.pdb, resolved inside the USB root.
		if rel, safe := prolink.SafeRel(t.Path); ok && safe {
			http.ServeFile(w, r, filepath.Join(s.usb, rel))
			return
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
			Master   *bool    `json:"master"`
			Sync     *bool    `json:"sync"`
			Nudge    *float64 `json:"nudge"`    // held pitch bend, percent
			ShiftMS  float64  `json:"shift_ms"` // jog wheel: move the play head
			HotCue   string   `json:"hot_cue"`  // A-H: jump if set, else set here
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
					s.cur, s.posMS, s.cueMS = i, 0, 0
				}
			}
		}
		if c.Master != nil {
			s.master = *c.Master
		}
		if c.Sync != nil {
			s.syncSnap = *c.Sync && !s.syncOn
			s.syncOn = *c.Sync
		}
		if c.Nudge != nil {
			s.nudge = math.Max(-16, math.Min(16, *c.Nudge))
		}
		dur := float64(s.tracks[s.cur].DurationMS)
		s.posMS = math.Max(0, math.Min(dur-1, s.posMS+c.ShiftMS))
		// CDJ CUE: playing returns to the cue point and pauses; paused sets the cue point here.
		if c.Cue && s.playing {
			s.posMS, s.playing = s.cueMS, false
		} else if c.Cue {
			s.cueMS = s.posMS
		}
		if len(c.HotCue) == 1 && c.HotCue >= "A" && c.HotCue <= "H" {
			id := s.tracks[s.cur].ID
			if s.hotCues[id] == nil {
				s.hotCues[id] = map[string]float64{}
			}
			if at, ok := s.hotCues[id][c.HotCue]; ok {
				s.posMS = at
			} else {
				s.hotCues[id][c.HotCue] = s.posMS
			}
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

// deckState is the deck as the UI shows it; full adds the track and addresses. Caller holds s.mu.
func (s *sim) deckState(full bool) map[string]any {
	beat, bib := s.beatAt()
	m := map[string]any{"number": s.number, "playing": s.playing, "pitch": s.pitch, "pitch_max": s.pitchMax,
		"track_id": s.tracks[s.cur].ID, "position_ms": s.nowPos(), "beat": beat, "beat_in_bar": bib, "bpm": s.bpm(),
		"effective_bpm": s.bpm() * (1 + s.pitch/100), "master": s.master, "sync": s.syncOn, "nudge": s.nudge,
		"cue_ms": s.cueMS, "hot_cues": s.hotCues[s.tracks[s.cur].ID], "master_bpm": s.masterBPM,
		"master_live": time.Since(s.masterSeen) < 2*time.Second, "phase_live": time.Since(s.masterAt) < 2*time.Second}
	if full {
		m["track"], m["ip"], m["portmap"] = s.tracks[s.cur], s.simIP.String(), s.nfs.PortmapPort()
	}
	return m
}

// track finds a track by ID; LINK loads can replace entries while handlers run.
func (s *sim) track(id uint32) (track, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tracks {
		if t.ID == id {
			return t, true
		}
	}
	return track{}, false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
