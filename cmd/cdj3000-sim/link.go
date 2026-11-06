package main

// Reverse direction (#63, on hold until a real CDJ is available): EngineX's
// Exporter makes the Denon drive look like a CDJ's USB, and the simulated
// CDJ-3000 browses it with its own dbserver client (written from the protocol
// analysis, not from pkg/prolink's encoder) and loads tracks over NFSv2.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/Cd1s/OpenProLink/pkg/cdj"
	"github.com/Cd1s/OpenProLink/third_party/enginex/prolink"
	"github.com/Cd1s/OpenProLink/third_party/enginex/rekordbox"
)

// exportIP is where the Denon export runs. The Exporter only takes an address
// an interface really has, and on loopback that is 127.0.0.1 alone.
var exportIP = net.IPv4(127, 0, 0, 1).To4()

func (s *sim) startExport() error {
	at := func(port string) string { return net.JoinHostPort(exportIP.String(), port) }
	s.exporter = prolink.NewExporter(prolink.ExportConfig{
		Root: s.denonDrive, IP: exportIP, MAC: net.HardwareAddr{2, 0, 0, 0, 0, 0x10},
		// Random ports: the Denon receiver may already hold 50000-50002 here.
		DBAddr: at("0"), PortmapAddr: at("0"), DiscoveryAddr: at("0"), StatusAddr: at("0"),
		AnnounceAddr:  net.JoinHostPort(s.simIP.String(), "50000"),
		ProbeDuration: time.Second,
	})
	return s.exporter.SetEnabled(true)
}

// listenDenon receives what the Denon export sends the CDJ: keepalives on
// 50000 (its player number) and status on 50002.
func (s *sim) listenDenon(ctx context.Context, port int) error {
	addr, lc := net.JoinHostPort(s.simIP.String(), strconv.Itoa(port)), net.ListenConfig{}
	if s.deck != nil {
		// A real deck broadcasts keepalives and may broadcast beats; on macOS only a
		// wildcard socket sees broadcasts, and it shares the port with EngineX's receiver.
		addr, lc.Control = net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), cdj.ReusePort
	}
	pc, err := lc.ListenPacket(ctx, "udp4", addr)
	if err != nil {
		return err
	}
	c := pc.(*net.UDPConn)
	context.AfterFunc(ctx, func() { c.Close() })
	go func() {
		defer c.Close()
		b := make([]byte, 2048)
		for {
			n, addr, err := c.ReadFromUDP(b)
			if err != nil {
				return
			}
			p := b[:n]
			if n < 0x25 || !bytes.HasPrefix(p, cdj.MagicHeader) {
				continue
			}
			kind, yielded := fmt.Sprintf("0x%02x", p[10]), byte(0)
			s.mu.Lock()
			simNum := s.number
			trackCount := uint16(len(s.tracks))
			if port == cdj.PortDiscovery && p[10] == cdj.TypeKeepalive && n >= 0x30 {
				if kp, err := cdj.ParseKeepalivePacket(p); err == nil {
					kind = fmt.Sprintf("keepalive 0x06, player %d", kp.PlayerNumber)
					// Only the first -deck is "the Denon": with several, each would take the slot in turn.
					if old := s.denonPlayer; old != kp.PlayerNumber && (s.deck == nil || addr.IP.Equal(s.deck)) {
						if old != 0 {
							s.yields = append(s.yields, fmt.Sprintf("%s Denon player %d → %d", time.Now().Format("15:04:05.000"), old, kp.PlayerNumber))
						}
						s.denonPlayer = kp.PlayerNumber
					}
				}
			} else if port == 50001 && p[10] == 0x28 {
				// Beat packets mark the master's downbeats and beats exactly, as a CDJ syncs to them.
				// From the -deck deck always (its exported player speaks for Engine's lead deck), else only from a master.
				fromDeck := s.isDeck(addr.IP)
				if b, err := cdj.ParseBeatPacket(p); err == nil && b.PlayerNumber != simNum && (fromDeck || s.peerMaster[b.PlayerNumber]) &&
					b.BeatWithinBar >= 1 && b.BeatWithinBar <= 4 && b.BPM > 0 {
					s.sawMasterBeat(b.BPM*(1+b.Pitch/100), float64(b.BeatWithinBar-1), true, time.Now())
					kind = fmt.Sprintf("beat 0x28 from player %d, beat %d", b.PlayerNumber, b.BeatWithinBar)
				}
			} else if port == cdj.PortStatus {
				// Another player's status (on the LAN, only the real deck's): BEAT SYNC source and the one-master rule.
				if st, err := cdj.ParseStatusPacket(p); err == nil && st.PlayerNumber != simNum && (s.deck == nil || s.isDeck(addr.IP)) {
					if st.Master && st.BPM > 0 {
						s.masterBPM, s.masterSeen = st.BPM*(1+st.Pitch/100), time.Now()
					}
					// Yield when it becomes master, not while it still is: pressing MASTER here takes it back,
					// and the deck's next status (still flagged until it lets go) must not undo that.
					if st.Master && !s.peerMaster[st.PlayerNumber] && s.master {
						s.master, yielded = false, st.PlayerNumber
					}
					s.peerMaster[st.PlayerNumber] = st.Master
				}
				switch st, err := cdj.ParseStatusPacket(p); {
				case p[10] == cdj.TypeMediaQuery:
					kind = fmt.Sprintf("media query 0x05, %d bytes", n)
				case err == nil:
					kind = fmt.Sprintf("status 0x0a from %s, player %d, master %v, %d bytes", addr.IP, st.PlayerNumber, st.Master, n)
				default:
					kind = fmt.Sprintf("status 0x%02x from %s, %d bytes", p[10], addr.IP, n)
				}
			}
			s.mu.Unlock()
			s.log("Denon → CDJ", port, kind, p)
			if yielded != 0 {
				s.log("CDJ", 0, fmt.Sprintf("MASTER yielded to player %d", yielded), nil)
			}

			if port == cdj.PortStatus && p[10] == cdj.TypeMediaQuery {
				if q, err := cdj.ParseMediaQueryPacket(p); err == nil {
					if (q.TargetPlayer == simNum || q.TargetPlayer == 0) && q.TargetSlot == cdj.SlotUSB {
						resp := cdj.NewMediaResponsePacket(cdj.MediaResponse{
							DeviceName:    "CDJ-3000",
							PlayerNumber:  simNum,
							TargetPlayer:  simNum,
							TargetSlot:    cdj.SlotUSB,
							MediaName:     "OPENCDJ-USB",
							CreatedDate:   "2026-11-05",
							TrackCount:    trackCount,
							PlaylistCount: 1,
							UIColor:       6,
							TrackType:     cdj.TrackTypeRekordbox,
							HasSettings:   true,
							TotalBytes:    32 << 30,
							FreeBytes:     28 << 30,
						})
						replyAddr := addr
						if replyAddr == nil && q.ReplyIP != nil && !q.ReplyIP.IsUnspecified() {
							replyAddr = &net.UDPAddr{IP: q.ReplyIP, Port: cdj.PortStatus}
						}
						if replyAddr != nil {
							_, _ = c.WriteToUDP(resp, replyAddr)
							s.log("CDJ → Denon", cdj.PortStatus, "media response 0x06", resp)
						}
					}
				}
			}
		}
	}()
	return nil
}

type dbConn struct {
	c    net.Conn
	tx   uint32
	dmst uint32
	s    *sim
}

func (d *dbConn) send(kind uint16, args ...uint32) error {
	d.tx++
	b := cdj.EncodeDBRequest(d.tx, kind, args...)
	d.s.log("CDJ → Denon", cdj.PortDBServer, fmt.Sprintf("dbserver request 0x%04x", kind), b)
	_, err := d.c.Write(b)
	return err
}

// dialDB does what a CDJ does: ask TCP 12523 for the dbserver port, connect,
// greet, and introduce itself with its player number.
func (s *sim) dialDB(addr string, me byte) (*dbConn, error) {
	q, err := net.DialTimeout("tcp4", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = q.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = q.Write(append([]byte{0, 0, 0, 15}, "RemoteDBServer\x00"...))
	var port [2]byte
	_, err = io.ReadFull(q, port[:])
	q.Close()
	if err != nil {
		return nil, fmt.Errorf("port query: %w", err)
	}
	host, _, _ := net.SplitHostPort(addr)
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	d := &dbConn{c: c, tx: 0, dmst: cdj.BuildDMST(me, 1, cdj.SlotUSB, cdj.TrackTypeRekordbox), s: s}
	var greeting [5]byte
	if _, err = c.Write([]byte{0x11, 0, 0, 0, 1}); err == nil {
		_, err = io.ReadFull(c, greeting[:])
	}
	if err != nil || greeting != [5]byte{0x11, 0, 0, 0, 1} {
		c.Close()
		return nil, fmt.Errorf("greeting: %v %x", err, greeting)
	}
	d.tx = 0xfffffffe - 1 // setup uses 0xfffffffe
	if err = d.send(cdj.KindSetup, uint32(me)); err == nil {
		var m *cdj.DBMessage
		if m, err = cdj.DecodeDBMessage(d.c); err == nil && (m.Kind != cdj.KindSetupAck || len(m.Args) < 2) {
			err = fmt.Errorf("setup answered 0x%04x", m.Kind)
		} else if err == nil {
			s.log("Denon → CDJ", cdj.PortDBServer, fmt.Sprintf("dbserver setup: Denon is player %d", m.Args[1].Num), nil)
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	d.tx = 0
	return d, nil
}

type linkItem struct {
	ID        uint32 `json:"id"`
	Kind      uint32 `json:"kind"`
	Label     string `json:"label"`
	Secondary string `json:"secondary,omitempty"`
}

// menu runs one query and renders all of its items, 64 at a time.
func (d *dbConn) menu(kind uint16, args ...uint32) ([]linkItem, error) {
	if err := d.send(kind, append([]uint32{d.dmst}, args...)...); err != nil {
		return nil, err
	}
	m, err := cdj.DecodeDBMessage(d.c)
	if err != nil {
		return nil, err
	}
	if m.Kind != cdj.KindSetupAck || len(m.Args) < 2 {
		return nil, fmt.Errorf("dbserver 0x%04x answered 0x%04x", kind, m.Kind)
	}
	count := m.Args[1].Num
	var items []linkItem
	for off := uint32(0); off < count; off += 64 {
		n := min(64, count-off)
		if err := d.send(cdj.KindRender, d.dmst, off, n, 0, count, 0); err != nil {
			return nil, err
		}
		if m, err = cdj.DecodeDBMessage(d.c); err != nil || m.Kind != cdj.KindRenderHeader {
			return nil, fmt.Errorf("render header 0x%04x %v", m.Kind, err)
		}
		for range n {
			if m, err = cdj.DecodeDBMessage(d.c); err != nil || m.Kind != cdj.KindRenderItem {
				return nil, fmt.Errorf("render item 0x%04x %v", m.Kind, err)
			}
			item, err := cdj.ParseMenuItem(m)
			if err != nil {
				return nil, fmt.Errorf("render item parse %v", err)
			}
			items = append(items, linkItem{ID: item.ID, Kind: item.Kind, Label: item.Label, Secondary: item.Secondary})
		}
		if m, err = cdj.DecodeDBMessage(d.c); err != nil || m.Kind != cdj.KindRenderFooter {
			return nil, fmt.Errorf("render footer 0x%04x %v", m.Kind, err)
		}
	}
	d.s.log("Denon → CDJ", cdj.PortDBServer, fmt.Sprintf("dbserver 0x%04x: %d items", kind, len(items)), nil)
	return items, nil
}

func (s *sim) linkDial() (*dbConn, prolink.ExportStatus, error) {
	st := s.exporter.Status()
	if s.deck != nil {
		// A real deck: its export answers on the standard ports (dbserver 12523, portmap 111).
		s.mu.Lock()
		st = prolink.ExportStatus{Enabled: true, Number: s.denonPlayer, DBAddress: net.JoinHostPort(s.deck.String(), "12523"), PortmapPort: 111}
		s.mu.Unlock()
	} else if !st.Enabled || st.Number == 0 {
		return nil, st, fmt.Errorf("Denon export is not answering yet (enabled %v, player %d)", st.Enabled, st.Number)
	}
	s.mu.Lock()
	me := s.number
	s.mu.Unlock()
	d, err := s.dialDB(st.DBAddress, me)
	return d, st, err
}

func (s *sim) linkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/export", func(w http.ResponseWriter, r *http.Request) {
		on := r.URL.Query().Get("enabled") == "1"
		if err := s.exporter.SetEnabled(on); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/link/menu", func(w http.ResponseWriter, r *http.Request) {
		d, _, err := s.linkDial()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer d.c.Close()
		id, _ := strconv.ParseUint(r.URL.Query().Get("id"), 10, 32)
		var items []linkItem
		switch r.URL.Query().Get("q") {
		case "root":
			items, err = d.menu(cdj.KindRootMenu, 0, 0xffffff)
		case "tracks":
			items, err = d.menu(cdj.KindAllTracks, 0)
		case "folder":
			items, err = d.menu(cdj.KindPlaylist, 0, uint32(id), 1)
		case "playlist":
			items, err = d.menu(cdj.KindPlaylist, 0, uint32(id), 0)
		default:
			err = fmt.Errorf("unknown menu")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, items)
	})
	mux.HandleFunc("POST /api/link/load", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.linkLoad(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/link/audio", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		name, data := s.linkName, s.linkAudio
		s.mu.Unlock()
		if data == nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}

// linkLoad loads a Denon track the way a CDJ does: path from dbserver, then
// export.pdb, ANLZ and audio over NFSv2.
func (s *sim) linkLoad(r *http.Request) (map[string]any, error) {
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 32)
	if err != nil {
		return nil, err
	}
	d, st, err := s.linkDial()
	if err != nil {
		return nil, err
	}
	items, err := d.menu(cdj.KindTrackInfo, uint32(id))
	d.c.Close()
	if err != nil || len(items) == 0 {
		return nil, fmt.Errorf("track path: %v", err)
	}
	audioPath := items[0].Label
	nfsIP := exportIP
	if s.deck != nil {
		nfsIP = s.deck
	}
	nfs := &prolink.NFSClient{IP: nfsIP, Portmap: st.PortmapPort}
	fetch := func(p string, max uint32) ([]byte, error) {
		b, _, err := nfs.Fetch(r.Context(), "/C/", p, max)
		if err == nil {
			s.log("Denon → CDJ", 2049, "NFSv2 READ "+p, b)
		}
		return b, err
	}
	// export.pdb is megabytes on a real stick: parse it once per size and mtime.
	const pdbPath = "/PIONEER/rekordbox/export.pdb"
	_, attr, err := nfs.Lookup(r.Context(), "/C/", pdbPath)
	if err != nil {
		return nil, fmt.Errorf("export.pdb over NFS: %w", err)
	}
	s.mu.Lock()
	lib := s.linkLib
	if attr != s.linkPDB {
		lib = nil
	}
	s.mu.Unlock()
	if lib == nil {
		pdb, err := fetch(pdbPath, 64<<20)
		if err != nil {
			return nil, fmt.Errorf("export.pdb over NFS: %w", err)
		}
		if lib, err = rekordbox.ParsePDB(bytes.NewReader(pdb), int64(len(pdb))); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.linkLib, s.linkPDB = lib, attr
		s.mu.Unlock()
	}
	var t rekordbox.Track
	for _, x := range lib.Tracks {
		if x.ID == uint32(id) {
			t = x
		}
	}
	tr := track{ID: remoteTrack | t.ID, RemoteID: t.ID, Remote: true, Title: t.Title, Artist: t.Artist, BPM: t.BPM, Path: audioPath,
		DurationMS: uint32(t.Duration * 1000)}
	if len(t.AnalysisPath) > 4 {
		stem := t.AnalysisPath[:len(t.AnalysisPath)-4]
		for _, ext := range []string{".2EX", ".EXT", ".DAT"} {
			b, err := fetch(stem+ext, 16<<20)
			if err != nil {
				continue
			}
			if a, err := rekordbox.ParseAnalysis(b); err == nil {
				wf, beats, dur := prolink.ExtractANLZ(a)
				if full := pwv7(a); len(full) > 0 {
					wf = full
				}
				if len(tr.Waveform) == 0 {
					tr.Waveform = wf
				}
				if len(tr.Beats) == 0 && len(beats) > 0 {
					tr.Beats, tr.DurationMS = beats, dur
				}
			}
		}
	}
	if tr.DurationMS == 0 {
		return nil, fmt.Errorf("track %d: no duration in export.pdb or beatgrid", id)
	}
	// Onto the main deck, paused at the first beat as a CDJ auto-cues. One linked track at a time.
	s.mu.Lock()
	s.linkName, s.linkAudio, s.linkAudioErr = path.Base(audioPath), nil, ""
	if i := len(s.tracks) - 1; s.tracks[i].Remote {
		delete(s.hotCues, s.tracks[i].ID)
		s.tracks[i] = tr
	} else {
		s.tracks = append(s.tracks, tr)
	}
	s.cur, s.playing, s.posMS, s.cueMS = len(s.tracks)-1, false, 0, 0
	if len(tr.Beats) > 0 {
		s.posMS, s.cueMS = float64(tr.Beats[0].TimeMS), float64(tr.Beats[0].TimeMS)
	}
	s.mu.Unlock()
	// The deck needs only the analysis; the audio (megabytes at ~1 MB/s over NFSv2) is
	// for listening in the browser, so it follows in the background.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		b, _, err := nfs.Fetch(ctx, "/C/", audioPath, 256<<20)
		s.mu.Lock()
		if s.tracks[len(s.tracks)-1].ID != tr.ID { // replaced by a newer load meanwhile
			s.mu.Unlock()
			return
		}
		if err != nil {
			s.linkAudioErr = "audio over NFS: " + err.Error()
		} else {
			s.linkAudio = b
		}
		s.mu.Unlock()
		if err == nil {
			s.log("Denon → CDJ", 2049, fmt.Sprintf("NFSv2 READ %s: %d bytes", audioPath, len(b)), nil)
		}
	}()
	return map[string]any{"track": tr, "waveform": tr.Waveform, "beats": tr.Beats, "duration_ms": tr.DurationMS,
		"denon_player": st.Number}, nil
}

// watchDeckLead polls an EngineX deck's /peer/decks, which knows Engine's own
// sync lead even while its Pro DJ Link export still reports no master, and
// yields MASTER when one of its decks becomes the lead.
// shortcut: EngineX-only HTTP API; drop it once the deck's 50002 status carries
// the master flag (rb-bridge fix in progress upstream).
func (s *sim) watchDeckLead(ctx context.Context) {
	url := "http://" + net.JoinHostPort(s.deck.String(), "8080") + "/peer/decks"
	client := &http.Client{Timeout: 400 * time.Millisecond}
	was := map[int]bool{}
	for t := time.NewTicker(500 * time.Millisecond); ; {
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		sent := time.Now()
		r, err := client.Get(url)
		if err != nil {
			continue
		}
		at := sent.Add(time.Since(sent) / 2) // the deck read its position about half a round trip ago
		var body struct {
			Decks []struct {
				Deck    int      `json:"deck"`
				Master  bool     `json:"master"`
				Playing bool     `json:"playing"`
				BPM     float64  `json:"bpm"`
				Pitch   float64  `json:"pitch"`
				Beat    *float64 `json:"beat"` // beats into the bar, 0..4 (Engine's /Engine/DeckN/BeatNumber)
			} `json:"decks"`
		}
		err = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)
		r.Body.Close()
		if err != nil {
			continue
		}
		for _, d := range body.Decks {
			s.mu.Lock()
			if d.Master && d.Playing && d.BPM > 0 && d.Beat != nil && !math.IsNaN(*d.Beat) {
				s.sawMasterBeat(d.BPM*(1+d.Pitch/100), math.Mod(math.Mod(*d.Beat, 4)+4, 4), true, at)
			}
			yield := d.Master && !was[d.Deck] && s.master
			if yield {
				s.master = false
			}
			s.mu.Unlock()
			was[d.Deck] = d.Master
			if yield {
				s.log("CDJ", 0, fmt.Sprintf("MASTER yielded: Engine deck %d on %s took the lead", d.Deck, s.deck), nil)
			}
		}
	}
}
