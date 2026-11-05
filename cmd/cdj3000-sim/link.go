package main

// Reverse direction (#63, on hold until a real CDJ is available): EngineX's
// Exporter makes the Denon drive look like a CDJ's USB, and the simulated
// CDJ-3000 browses it with its own dbserver client (written from the protocol
// analysis, not from pkg/prolink's encoder) and loads tracks over NFSv2.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/Cd1s/OpenCDJ-3000/pkg/cdj"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/prolink"
	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
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
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: s.simIP, Port: port})
	if err != nil {
		return err
	}
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
			kind := fmt.Sprintf("0x%02x", p[10])
			s.mu.Lock()
			simNum := s.number
			trackCount := uint16(len(s.tracks))
			if port == cdj.PortDiscovery && p[10] == cdj.TypeKeepalive && n >= 0x30 {
				if kp, err := cdj.ParseKeepalivePacket(p); err == nil {
					kind = fmt.Sprintf("keepalive 0x06, player %d", kp.PlayerNumber)
					if old := s.denonPlayer; old != kp.PlayerNumber {
						if old != 0 {
							s.yields = append(s.yields, fmt.Sprintf("%s Denon player %d → %d", time.Now().Format("15:04:05.000"), old, kp.PlayerNumber))
						}
						s.denonPlayer = kp.PlayerNumber
					}
				}
			} else if port == cdj.PortStatus {
				if p[10] == cdj.TypeMediaQuery {
					kind = fmt.Sprintf("media query 0x05, %d bytes", n)
				} else {
					kind = fmt.Sprintf("status 0x%02x, %d bytes", p[10], n)
				}
			}
			s.mu.Unlock()
			s.log("Denon → CDJ", port, kind, p)

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
	if !st.Enabled || st.Number == 0 {
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
	nfs := &prolink.NFSClient{IP: exportIP, Portmap: st.PortmapPort}
	fetch := func(p string, max uint32) ([]byte, error) {
		b, _, err := nfs.Fetch(r.Context(), "/C/", p, max)
		if err == nil {
			s.log("Denon → CDJ", 2049, "NFSv2 READ "+p, b)
		}
		return b, err
	}
	pdb, err := fetch("/PIONEER/rekordbox/export.pdb", 64<<20)
	if err != nil {
		return nil, fmt.Errorf("export.pdb over NFS: %w", err)
	}
	lib, err := rekordbox.ParsePDB(bytes.NewReader(pdb), int64(len(pdb)))
	if err != nil {
		return nil, err
	}
	var t rekordbox.Track
	for _, x := range lib.Tracks {
		if x.ID == uint32(id) {
			t = x
		}
	}
	tr := track{ID: t.ID, Title: t.Title, Artist: t.Artist, BPM: t.BPM, Path: audioPath}
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
	audio, err := fetch(audioPath, 256<<20)
	if err != nil {
		return nil, fmt.Errorf("audio over NFS: %w", err)
	}
	s.mu.Lock()
	s.linkName, s.linkAudio = path.Base(audioPath), audio
	s.mu.Unlock()
	return map[string]any{"track": tr, "waveform": tr.Waveform, "beats": tr.Beats, "duration_ms": tr.DurationMS,
		"audio_bytes": len(audio), "denon_player": st.Number}, nil
}
