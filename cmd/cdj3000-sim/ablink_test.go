package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

func TestAbletonLinkPongTimelineAndPeers(t *testing.T) {
	udp := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	phase, follow, steered := 1.0, false, [2]float64{}
	l := &ableLink{id: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, start: time.Now(), tx: udp(), mep: udp(), peers: map[[8]byte]*linkPeer{},
		beat: func() (float64, float64, bool, bool) { return 120, phase, true, follow }}
	l.steer = func(bpm, ph float64) { steered = [2]float64{bpm, ph} }

	// Pong: header, our session, ghost time, then the ping payload echoed.
	ping := append(append([]byte{}, measureMagic...), linkPing)
	ping = append(ping, entry("__ht", int64(123456))...)
	pong := l.pong(ping)
	e := entries(pong[9:])
	if pong[8] != linkPong || !bytes.Equal(e["sess"], l.id[:]) || int64(binary.BigEndian.Uint64(e["__gt"])) < ghostLead ||
		!bytes.HasSuffix(pong, ping[9:]) {
		t.Fatalf("bad pong %x", pong)
	}
	if l.pong(append(ping, make([]byte, 40)...)) != nil {
		t.Fatal("oversized ping answered")
	}

	inBar := func() float64 { return math.Mod(l.timeline.beatsAt(l.ghost()), 4) }
	if !l.update() || l.timeline.microsPerBeat != 500000 || math.Abs(inBar()-1) > 0.01 {
		t.Fatalf("first timeline %+v, bar phase %v", l.timeline, inBar())
	}
	if l.update() {
		t.Fatal("re-anchored without a change")
	}
	first := l.timeline
	phase = 2.5 // nudged or cued: the bar phase moves, the beat origin only grows
	if !l.update() || l.timeline.beatOrigin <= first.beatOrigin || math.Abs(inBar()-2.5) > 0.01 {
		t.Fatalf("after nudge %+v, bar phase %v", l.timeline, inBar())
	}

	// A peer in our session publishes a newer timeline: we answer with a newer one still.
	peer := append(append([]byte{}, discoveryMagic...), linkAlive, linkTTL, 0, 0, 9, 9, 9, 9, 9, 9, 9, 9)
	peer = append(peer, entry("tmln", int64(400000), l.timeline.beatOrigin+8e6, l.ghost())...)
	peer = append(peer, entry("sess", l.id)...)
	l.receive(peer, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if st := l.status(); st["joined"] != 1 || st["peers"] != 1 {
		t.Fatalf("status %v", st)
	}
	if !l.update() || l.timeline.beatOrigin <= first.beatOrigin+8e6 || math.Abs(inBar()-2.5) > 0.01 {
		t.Fatalf("did not outrun the peer: %+v, bar phase %v", l.timeline, inBar())
	}

	// BEAT SYNC (follow): the deck changes tempo; we adopt its timeline and steer to it.
	follow = true
	deck := linkTimeline{400000, l.timeline.beatOrigin + 16e6, l.ghost()} // 150 BPM, bar phase 0
	peer = append(append([]byte{}, discoveryMagic...), linkAlive, linkTTL, 0, 0, 9, 9, 9, 9, 9, 9, 9, 9)
	peer = append(peer, entry("tmln", deck.microsPerBeat, deck.beatOrigin, deck.timeOrigin)...)
	peer = append(peer, entry("sess", l.id)...)
	l.receive(peer, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if l.update() || l.timeline != deck || steered[0] != 150 || math.Abs(math.Mod(deck.beatsAt(l.ghost()), 4)-steered[1]) > 0.01 {
		t.Fatalf("follow: timeline %+v steered %v", l.timeline, steered)
	}
}

func TestBeatSyncMatchesTempoAndPhase(t *testing.T) {
	s, err := newSim("", net.IPv4(127, 0, 0, 14).To4(), net.IPv4(127, 0, 0, 13).To4())
	if err != nil {
		t.Fatal(err)
	}
	// Fixture track 1: 120 BPM grid, beat 1 every 2 s. 10.1 s is 0.2 beats into a bar.
	s.playing, s.posMS, s.master, s.pitchMax = true, 10_100, false, 10
	s.syncOn, s.syncSnap = true, true
	now := time.Now()
	s.lastTick = now

	// Ableton Link session at 126 BPM, 1.2 beats into its bar: snap tempo and bar phase.
	s.linkSteer(126, 1.2)
	s.advance(s.masterAt)
	if math.Abs(s.pitch-5) > 1e-9 || math.Abs(s.barPhase(s.posMS)-1.2) > 0.005 {
		t.Fatalf("snap: pitch %v, bar phase %v", s.pitch, s.barPhase(s.posMS))
	}

	// Small drift (10 ms) is pulled in by a quarter per tick, not jumped.
	s.posMS -= 10
	s.lastTick = s.masterAt
	s.advance(s.masterAt)
	if drift := (1.2 - s.barPhase(s.posMS)) * 500; math.Abs(drift-7.5) > 0.1 {
		t.Fatalf("drift after one tick %.2f ms, want 7.5", drift)
	}

	// A master 12.5 % faster than ±10 allows: the range widens to ±16 and the tempo still matches.
	s.mu.Lock()
	s.sawMasterBeat(135, 0.5, false, time.Now()) // beat phase only: align to the beat, not the bar
	s.mu.Unlock()
	s.lastTick = s.masterAt
	s.advance(s.masterAt)
	if s.pitchMax != 16 || math.Abs(s.pitch-12.5) > 1e-9 {
		t.Fatalf("range %v pitch %v, want 16 and 12.5", s.pitchMax, s.pitch)
	}
	s.syncSnap = true
	s.advance(s.masterAt)
	if f := math.Mod(s.barPhase(s.posMS), 1); math.Abs(f-0.5) > 0.005 {
		t.Fatalf("beat phase %v, want 0.5", f)
	}

	// MASTER on: it leads and no longer follows.
	s.master, s.pitch = true, 0
	s.advance(s.masterAt)
	if s.pitch != 0 {
		t.Fatalf("a master followed: pitch %v", s.pitch)
	}
}
