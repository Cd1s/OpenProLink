package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/OpenProLink/pkg/cdj"
	"github.com/Cd1s/OpenProLink/third_party/enginex/bridge"
	"github.com/Cd1s/OpenProLink/third_party/enginex/enginedb"
	"github.com/Cd1s/OpenProLink/third_party/enginex/prolink"
)

// Own loopback addresses: pkg/prolink's tests hold 127.0.0.1:50000-50002.
func TestSimulatorReachesEngineX(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 6).To4(), net.IPv4(127, 0, 0, 5).To4())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.number, s.playing, s.pitch = 2, true, 3.125
	s.mu.Unlock()

	var seen prolink.Device
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, d := range s.discovery.Devices(time.Now()) {
			if d.Number == 2 && d.Name == "CDJ-3000" && d.Playing && len(d.Waveform) > 0 && len(d.Beats) > 0 {
				seen = d
			}
		}
		if seen.Number != 0 {
			break
		}
	}
	if seen.Number == 0 {
		t.Fatalf("EngineX never saw a playing CDJ-3000 with NFS waveform: %+v", s.discovery.Devices(time.Now()))
	}
	// 0x0b position packets carry pitch in 0.01 % steps, status packets exactly; either may be the last one parsed.
	if seen.TrackID != s.tracks[0].ID || math.Abs(seen.Pitch-3.125) > 0.005 || seen.Title == "" {
		t.Fatalf("EngineX parsed %+v", seen)
	}
	if !seen.PrecisePosition {
		t.Fatalf("EngineX did not receive or parse 0x0b PrecisePosition: %+v", seen)
	}

	// Verify MediaQuery (0x05) -> MediaResponse (0x06) to s.simIP:50002
	queryConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: s.denonIP, Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP for media query failed: %v", err)
	}
	defer queryConn.Close()
	_ = queryConn.SetDeadline(time.Now().Add(2 * time.Second))

	queryPkt := cdj.NewMediaQueryPacket(cdj.MediaQuery{
		DeviceName:   "CDJ-3000",
		PlayerNumber: 1,
		ReplyIP:      s.denonIP,
		TargetPlayer: 2,
		TargetSlot:   cdj.SlotUSB,
	})
	if _, err := queryConn.WriteToUDP(queryPkt, &net.UDPAddr{IP: s.simIP, Port: 50002}); err != nil {
		t.Fatalf("WriteToUDP media query failed: %v", err)
	}

	respBuf := make([]byte, 512)
	n, _, err := queryConn.ReadFromUDP(respBuf)
	if err != nil {
		t.Fatalf("ReadFromUDP media response failed: %v", err)
	}
	resp, err := cdj.ParseMediaResponsePacket(respBuf[:n])
	if err != nil {
		t.Fatalf("ParseMediaResponsePacket failed: %v", err)
	}
	if resp.TrackCount != uint16(len(s.tracks)) {
		t.Fatalf("Media response TrackCount = %d, want %d", resp.TrackCount, len(s.tracks))
	}
	if resp.MediaName != "OPENCDJ-USB" {
		t.Fatalf("Media response MediaName = %q, want OPENCDJ-USB", resp.MediaName)
	}

	out, err := s.stage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "[Pro DJ Link: CDJ-3000]") {
		t.Fatalf("no CDJ playlist folder after #59 import: %s", b)
	}

	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))
	var state struct {
		Denon []map[string]any `json:"denon"`
		Wire  []wireEntry      `json:"wire"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || len(state.Denon) == 0 || len(state.Wire) == 0 {
		t.Fatalf("state %d %v: %.300s", w.Code, err, w.Body)
	}
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("POST", "/api/control", strings.NewReader(`{"pitch_max":6,"pitch":9}`)))
	if s.pitch != 6 {
		t.Fatalf("pitch not clamped to range: %v", s.pitch)
	}

	if n := len(s.tracks[0].Waveform); n < 600 || len(s.tracks[0].Beats) != 128 {
		t.Fatalf("drum track: %d waveform columns, %d beats", n, len(s.tracks[0].Beats))
	}
}

// #63: browse and load from the Denon export, and watch it yield its number.
func TestSimulatorReverseLinkAndYield(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 8).To4(), net.IPv4(127, 0, 0, 7).To4())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	wait("Denon export to take player 4", func() bool { return s.exporter.Status().Number == 4 })
	mux := s.routes()
	get := func(method, url string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, url, nil))
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, url, w.Code, w.Body)
		}
		return w
	}
	var root []linkItem
	json.Unmarshal(get("GET", "/api/link/menu?q=root").Body.Bytes(), &root)
	if len(root) != 2 || root[0].Label != "PLAYLIST" {
		t.Fatalf("root menu %+v", root)
	}
	var tracks []linkItem
	json.Unmarshal(get("GET", "/api/link/menu?q=tracks").Body.Bytes(), &tracks)
	if len(tracks) != 5 {
		t.Fatalf("tracks %+v", tracks)
	}
	var loaded struct {
		Waveform [][3]byte `json:"waveform"`
		Beats    []any     `json:"beats"`
	}
	json.Unmarshal(get("POST", "/api/link/load?id=1").Body.Bytes(), &loaded)
	if len(loaded.Waveform) < 600 || len(loaded.Beats) != 128 {
		t.Fatalf("loaded %d columns, %d beats", len(loaded.Waveform), len(loaded.Beats))
	}
	// On the main deck, reported as the Denon's track; the audio follows in the background.
	s.mu.Lock()
	cur, status := s.tracks[s.cur], s.statusPacket(1, 1)
	s.mu.Unlock()
	if !cur.Remote || cur.RemoteID != 1 || status[0x28] != s.denonPlayer || binary.BigEndian.Uint32(status[0x2c:]) != 1 {
		t.Fatalf("main deck %+v, status source %d track %d", cur, status[0x28], binary.BigEndian.Uint32(status[0x2c:]))
	}
	wait("LINK audio download", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.linkAudio) > 1<<20 })
	if w := get("GET", fmt.Sprintf("/audio?id=%d", cur.ID)); w.Body.Len() < 1<<20 {
		t.Fatal("audio not served on the main deck")
	}
	if json.Unmarshal(get("POST", "/api/link/load?id=1").Body.Bytes(), &loaded); len(s.tracks) != 6 {
		t.Fatalf("a second LINK load added a track: %d", len(s.tracks))
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/control", strings.NewReader(`{"number":4}`)))
	wait("Denon export to yield to player 3", func() bool { return s.exporter.Status().Number == 3 })
	wait("the yield to show in the CDJ's view", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.denonPlayer == 3 && len(s.yields) > 0 && strings.Contains(fmt.Sprint(s.yields), "4 → 3")
	})
}

func TestSimulatorEmptyDenonDriveStage(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 10).To4(), net.IPv4(127, 0, 0, 9).To4())
	if err != nil {
		t.Fatal(err)
	}
	s.denonEmpty = true
	if err := s.start(t.Context()); err != nil {
		t.Fatal(err)
	}

	out, err := s.stage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "[Pro DJ Link: CDJ-3000]") {
		t.Fatalf("no CDJ playlist folder after #59 import on empty Denon drive: %s", b)
	}

	if s.exporter.Status().Enabled {
		t.Fatalf("empty Denon drive should not have an enabled exporter: %+v", s.exporter.Status())
	}

	res, err := bridge.ConvertAtMount(t.Context(), s.denonDrive, enginedb.Options{}, false)
	if err != nil {
		t.Fatalf("ConvertAtMount failed: %v", err)
	}
	if !res.Bypassed {
		t.Fatalf("expected unchanged mount to be bypassed, got %+v", res)
	}
}

func TestDeckControlsMasterSyncCuesNudge(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 12).To4(), net.IPv4(127, 0, 0, 11).To4())
	if err != nil {
		t.Fatal(err)
	}
	s.master = true
	mux := s.routes()
	ctl := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/control", strings.NewReader(body)))
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	s.nfs = &prolink.NFSServer{Root: s.usb} // deckState asks its port for /api/state only
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/tick", nil))
	if w.Body.Len() > 600 || !strings.Contains(w.Body.String(), `"track_id":1`) {
		t.Fatalf("tick %d bytes: %s", w.Body.Len(), w.Body)
	}
	flags := func() byte { return s.statusPacket(1, 1)[0x89] }
	if flags()&cdj.FlagMaster == 0 || flags()&cdj.FlagSync != 0 {
		t.Fatalf("default flags %#x, want master only", flags())
	}
	ctl(`{"master":false,"sync":true}`)
	if flags()&cdj.FlagMaster != 0 || flags()&cdj.FlagSync == 0 {
		t.Fatalf("flags %#x, want sync only", flags())
	}

	// Paused CUE sets the cue point; playing CUE returns there and pauses.
	ctl(`{"playing":false,"shift_ms":4000}`)
	ctl(`{"cue":true}`)
	ctl(`{"playing":true,"shift_ms":2000}`)
	ctl(`{"cue":true}`)
	if s.playing || s.posMS != 4000 || s.cueMS != 4000 {
		t.Fatalf("cue: playing %v pos %v cue %v", s.playing, s.posMS, s.cueMS)
	}
	// Hot cue: first press stores, later press jumps back.
	ctl(`{"hot_cue":"B"}`)
	ctl(`{"shift_ms":1500}`)
	ctl(`{"hot_cue":"B"}`)
	ctl(`{"hot_cue":"Z"}`)
	if s.posMS != 4000 || len(s.hotCues[s.tracks[s.cur].ID]) != 1 {
		t.Fatalf("hot cue: pos %v cues %v", s.posMS, s.hotCues)
	}

	// Nudge moves the play head faster without changing the reported pitch.
	ctl(`{"playing":true,"nudge":5}`)
	t0 := time.Now()
	s.lastTick = t0
	s.advance(t0.Add(time.Second))
	if math.Abs(s.posMS-5050) > 1e-6 || s.pitch != 0 {
		t.Fatalf("nudge: pos %v pitch %v, want 5050 and 0", s.posMS, s.pitch)
	}

	// BEAT SYNC follows a live master's effective BPM, clamped to the range.
	s.masterBPM, s.masterSeen = s.bpm()*1.03, time.Now()
	s.advance(time.Now())
	if math.Abs(s.pitch-3) > 1e-9 {
		t.Fatalf("sync pitch %v, want 3", s.pitch)
	}
}

func TestOneMasterOnTheLink(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 16).To4(), net.IPv4(127, 0, 0, 15).To4())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.master = true
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	deck, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: s.simIP, Port: 50002})
	if err != nil {
		t.Fatal(err)
	}
	defer deck.Close()
	beats, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: s.simIP, Port: 50001})
	if err != nil {
		t.Fatal(err)
	}
	defer beats.Close()
	status := func(master bool) {
		_, _ = deck.Write(cdj.NewCDJ3000StatusPacket(cdj.Status{DeviceName: "SC5000", PlayerNumber: 1, Master: master, BPM: 124, TrackID: 1}))
	}
	isMaster := func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.master }
	settle := func(want bool, what string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if isMaster() == want {
				return
			}
		}
		t.Fatalf("%s: master %v", what, isMaster())
	}

	status(true)
	settle(false, "deck became master, the CDJ must yield")
	s.mu.Lock()
	s.master = true // MASTER pressed on the CDJ
	s.mu.Unlock()
	status(true) // the deck has not let go yet
	time.Sleep(200 * time.Millisecond)
	if !isMaster() {
		t.Fatal("a deck that was already master took MASTER back")
	}
	status(false)
	status(true)
	settle(false, "deck became master again")

	// The master's beat packets give BEAT SYNC its phase: beat 3 of the bar, now.
	_, _ = beats.Write(cdj.NewBeatPacket(cdj.Beat{DeviceName: "SC5000", PlayerNumber: 1, BPM: 124, BeatWithinBar: 3}))
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		s.mu.Lock()
		got := s.barKnown && s.masterBar == 2 && time.Since(s.masterAt) < time.Second
		s.mu.Unlock()
		if got {
			return
		}
	}
	t.Fatal("beat packet from the master did not set the phase")
}

func TestDeckLeadFromPeerDecks(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 18).To4(), net.IPv4(127, 0, 0, 17).To4())
	if err != nil {
		t.Fatal(err)
	}
	s.deck, s.master = net.IPv4(127, 0, 0, 19).To4(), true
	ln, err := net.Listen("tcp4", "127.0.0.19:8080")
	if err != nil {
		t.Skip("127.0.0.19:8080 busy:", err)
	}
	var lead atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"decks":[{"deck":1,"playing":false,"bpm":110},{"deck":3,"playing":true,"master":%v,"bpm":124,"pitch":0.5,"beat":2.25}]}`, lead.Load())
	})}
	go srv.Serve(ln)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchDeckLead(ctx)
	isMaster := func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.master }

	time.Sleep(700 * time.Millisecond)
	if !isMaster() {
		t.Fatal("yielded while no Engine deck leads")
	}
	lead.Store(true)
	for deadline := time.Now().Add(2 * time.Second); isMaster() && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if isMaster() {
		t.Fatal("Engine deck 3 took the lead but the CDJ kept MASTER")
	}
	s.mu.Lock()
	bpm, bar, known, fresh := s.masterBPM, s.masterBar, s.barKnown, time.Since(s.masterAt) < time.Second
	s.mu.Unlock()
	if math.Abs(bpm-124.62) > 1e-9 || bar != 2.25 || !known || !fresh {
		t.Fatalf("lead deck phase: bpm %v bar %v known %v fresh %v", bpm, bar, known, fresh)
	}
	s.mu.Lock()
	s.master = true // MASTER pressed here while deck 3 still leads
	s.mu.Unlock()
	time.Sleep(700 * time.Millisecond)
	if !isMaster() {
		t.Fatal("a deck that already led took MASTER back")
	}
}

// Several -deck decks each get the CDJ's packets, and each is heard as a deck.
func TestEveryDeckIsATarget(t *testing.T) {
	a, b := net.IPv4(192, 168, 50, 12).To4(), net.IPv4(192, 168, 50, 26).To4()
	s := &sim{deck: a, decks: []net.IP{a, b}}
	if got := s.targets(); len(got) != 2 || !got[0].Equal(a) || !got[1].Equal(b) {
		t.Fatalf("targets %v", got)
	}
	if !s.isDeck(a) || !s.isDeck(b) || s.isDeck(net.IPv4(192, 168, 50, 9)) {
		t.Fatal("isDeck")
	}
}
