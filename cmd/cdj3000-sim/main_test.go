package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/prolink"
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
	if seen.TrackID != s.tracks[0].ID || seen.Pitch != 3.125 || seen.Title == "" {
		t.Fatalf("EngineX parsed %+v", seen)
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
		Waveform   [][3]byte `json:"waveform"`
		Beats      []any     `json:"beats"`
		AudioBytes int       `json:"audio_bytes"`
	}
	json.Unmarshal(get("POST", "/api/link/load?id=1").Body.Bytes(), &loaded)
	if len(loaded.Waveform) < 600 || len(loaded.Beats) != 128 || loaded.AudioBytes < 1<<20 {
		t.Fatalf("loaded %d columns, %d beats, %d bytes", len(loaded.Waveform), len(loaded.Beats), loaded.AudioBytes)
	}
	if w := get("GET", "/api/link/audio"); w.Body.Len() != loaded.AudioBytes {
		t.Fatal("audio not served")
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
