package main

// Ableton Link peer: publishes the virtual CDJ's tempo and bar phase as a Link
// session, so a deck with Link enabled (Engine OS has it: EnableAbletonLink)
// can SYNC to the simulator. Wire format from Ableton's open-source Link
// (discovery/v1 and link/v1 Messages.hpp, Timeline, PingResponder, Sessions):
// big-endian; entries are key(u32) size(u32) value.

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"syscall"
	"time"
)

var (
	linkGroup      = &net.UDPAddr{IP: net.IPv4(224, 76, 78, 75), Port: 20808}
	discoveryMagic = []byte("_asdp_v\x01")
	measureMagic   = []byte("_link_v\x01")
)

const (
	linkAlive, linkResponse, linkByeBye = 1, 2, 3
	linkPing, linkPong                  = 1, 2
	linkTTL                             = 5 // seconds; Link broadcasts every ttl/20
	// shortcut: our ghost clock starts 2^50 µs (~35 years) ahead, so any Link
	// session that measures ours finds it older and joins it (Sessions.hpp:
	// larger ghost time wins). The simulator never joins another session;
	// upgrade to Link's ping measurement if it ever has to follow one.
	ghostLead = int64(1) << 50
)

type linkTimeline struct {
	microsPerBeat, beatOrigin, timeOrigin int64 // µs/beat, micro-beats, ghost µs
}

func (t linkTimeline) beatsAt(ghost int64) float64 {
	return float64(t.beatOrigin)/1e6 + float64(ghost-t.timeOrigin)/float64(t.microsPerBeat)
}

type linkPeer struct {
	session  [8]byte
	timeline linkTimeline
	seen     time.Time
}

type ableLink struct {
	id    [8]byte // node ID, also our session ID
	start time.Time
	tx    *net.UDPConn // unicast: sends alive, receives responses
	mep   *net.UDPConn // measurement endpoint: answers pings

	mu       sync.Mutex
	timeline linkTimeline
	peers    map[[8]byte]*linkPeer
	beat     func() (bpm, phase float64, playing, follow bool) // phase: beats into the bar, [0, 4)
	steer    func(bpm, phase float64)                          // follow the session (BEAT SYNC, not MASTER)
}

func (l *ableLink) ghost() int64 { return time.Since(l.start).Microseconds() + ghostLead }

func entry(key string, v ...any) []byte {
	var b bytes.Buffer
	for _, x := range v {
		_ = binary.Write(&b, binary.BigEndian, x)
	}
	out := binary.BigEndian.AppendUint32([]byte(key), uint32(b.Len()))
	return append(out, b.Bytes()...)
}

func (l *ableLink) state() []byte {
	mep := l.mep.LocalAddr().(*net.UDPAddr)
	t := l.timeline
	p := entry("tmln", t.microsPerBeat, t.beatOrigin, t.timeOrigin)
	p = append(p, entry("sess", l.id)...)
	// Start/stop state with timestamp 0 never wins, so the simulator cannot start or stop the deck.
	p = append(p, entry("stst", false, int64(0), int64(0))...)
	return append(p, entry("mep4", binary.BigEndian.Uint32(mep.IP.To4()), uint16(mep.Port))...)
}

func (l *ableLink) message(kind byte) []byte {
	b := append(append([]byte{}, discoveryMagic...), kind, linkTTL, 0, 0)
	b = append(b, l.id[:]...)
	if kind == linkByeBye {
		return b
	}
	return append(b, l.state()...)
}

// entries splits a payload into key -> value, ignoring keys it does not know.
func entries(p []byte) map[string][]byte {
	m := map[string][]byte{}
	for len(p) >= 8 {
		n := binary.BigEndian.Uint32(p[4:8])
		if uint64(len(p)-8) < uint64(n) {
			break
		}
		m[string(p[:4])] = p[8 : 8+n]
		p = p[8+n:]
	}
	return m
}

func (l *ableLink) receive(b []byte, from *net.UDPAddr) {
	if len(b) < 20 || !bytes.Equal(b[:8], discoveryMagic) {
		return
	}
	var id [8]byte
	copy(id[:], b[12:20])
	if id == l.id || b[10] != 0 || b[11] != 0 { // ourselves, or another session group
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b[8] == linkByeBye {
		delete(l.peers, id)
		return
	}
	e := entries(b[20:])
	p := &linkPeer{seen: time.Now()}
	copy(p.session[:], e["sess"])
	if t := e["tmln"]; len(t) == 24 {
		p.timeline = linkTimeline{int64(binary.BigEndian.Uint64(t)), int64(binary.BigEndian.Uint64(t[8:])), int64(binary.BigEndian.Uint64(t[16:]))}
	}
	l.peers[id] = p
	if b[8] == linkAlive {
		_, _ = l.tx.WriteToUDP(l.message(linkResponse), from)
	}
}

// pong answers a measurement ping: our session, our ghost time, then the ping payload echoed.
func (l *ableLink) pong(b []byte) []byte {
	if len(b) < 9 || !bytes.Equal(b[:8], measureMagic) || b[8] != linkPing || len(b)-9 > 32 {
		return nil
	}
	out := append(append([]byte{}, measureMagic...), linkPong)
	out = append(out, entry("sess", l.id)...)
	out = append(out, entry("__gt", l.ghost())...)
	return append(out, b[9:]...)
}

// update leads or follows the session. Leading (MASTER, or no peer joined), it
// re-anchors our timeline when the tempo changed, the bar phase drifted more than
// 5 ms, or a peer published a newer one; peers adopt the timeline with the larger
// beat origin (Sessions::updateTimeline), so ours only ever moves forward.
// Following (BEAT SYNC), it adopts the newest peer timeline and steers the deck to it.
func (l *ableLink) update() bool {
	bpm, phase, playing, follow := l.beat()
	if bpm <= 0 {
		return false
	}
	g := l.ghost()
	l.mu.Lock()
	mpb := int64(math.Round(60e6 / bpm))
	t := l.timeline
	base := 0.0
	if t.microsPerBeat > 0 {
		base = t.beatsAt(g)
	}
	newer, joined := false, 0
	for id, p := range l.peers {
		if time.Since(p.seen) > linkTTL*time.Second {
			delete(l.peers, id)
			continue
		}
		if p.session == l.id {
			joined++
		}
		if p.session == l.id && p.timeline.beatOrigin > l.timeline.beatOrigin && p.timeline.microsPerBeat > 0 {
			newer = true
			base = math.Max(base, p.timeline.beatsAt(g))
			if follow {
				l.timeline = p.timeline
			}
		}
	}
	if follow && joined > 0 && l.timeline.microsPerBeat > 0 {
		tl := l.timeline
		l.mu.Unlock()
		l.steer(60e6/float64(tl.microsPerBeat), math.Mod(tl.beatsAt(g), 4))
		return false
	}
	defer l.mu.Unlock()
	drift := math.Mod(phase-base, 4)
	if drift < 0 {
		drift += 4
	}
	off := math.Min(drift, 4-drift) * float64(mpb) / 1000 // ms
	if t.microsPerBeat == mpb && !newer && (!playing || off < 5) {
		return false
	}
	if !playing {
		drift = 0 // paused: keep the tempo, leave the phase alone
	}
	if newer && drift < 1e-3 {
		drift += 4
	}
	l.timeline = linkTimeline{mpb, int64(math.Ceil((base + drift) * 1e6)), g}
	return true
}

func (l *ableLink) status() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	joined := 0
	for _, p := range l.peers {
		if p.session == l.id {
			joined++
		}
	}
	return map[string]any{"peers": len(l.peers), "joined": joined, "bpm": 60e6 / math.Max(1, float64(l.timeline.microsPerBeat))}
}

// startAbletonLink joins the Link multicast group on the interface that holds ip.
func (s *sim) startAbletonLink(ip net.IP) (*ableLink, error) {
	var ifi *net.Interface
	ifs, _ := net.Interfaces()
	for i := range ifs {
		addrs, _ := ifs[i].Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				ifi = &ifs[i]
			}
		}
	}
	if ifi == nil {
		return nil, fmt.Errorf("ableton link: no interface has %s", ip)
	}
	l := &ableLink{start: time.Now(), peers: map[[8]byte]*linkPeer{}, beat: s.linkBeat, steer: s.linkSteer}
	if _, err := rand.Read(l.id[:]); err != nil {
		return nil, err
	}
	rx, err := net.ListenMulticastUDP("udp4", ifi, linkGroup)
	if err != nil {
		return nil, fmt.Errorf("ableton link: %w", err)
	}
	if l.tx, err = net.ListenUDP("udp4", &net.UDPAddr{IP: ip}); err == nil {
		l.mep, err = net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	}
	if err == nil {
		// Send the multicast out of this interface, not whichever one the route picks.
		var raw syscall.RawConn
		if raw, err = l.tx.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				err = syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, [4]byte(ip.To4()))
			})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ableton link: %w", err)
	}
	read := func(c *net.UDPConn, handle func([]byte, *net.UDPAddr)) {
		b := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDP(b)
			if err != nil {
				return
			}
			handle(b[:n], from)
		}
	}
	go read(rx, l.receive)
	go read(l.tx, l.receive)
	go read(l.mep, func(b []byte, from *net.UDPAddr) {
		if out := l.pong(b); out != nil {
			_, _ = l.mep.WriteToUDP(out, from)
		}
	})
	l.update()
	go func() {
		var last time.Time
		for range time.Tick(20 * time.Millisecond) {
			if l.update() || time.Since(last) >= linkTTL*time.Second/20 {
				l.mu.Lock()
				msg := l.message(linkAlive)
				l.mu.Unlock()
				if _, err := l.tx.WriteToUDP(msg, linkGroup); err == nil {
					last = time.Now()
				}
			}
		}
	}()
	return l, nil
}

// nowPos is the play head now; s.posMS is as of the last 20 ms tick. Caller holds s.mu.
func (s *sim) nowPos() float64 {
	if s.playing && !s.lastTick.IsZero() {
		return s.posMS + float64(time.Since(s.lastTick).Microseconds())/1000*(1+(s.pitch+s.nudge)/100)
	}
	return s.posMS
}

// barPhase is how many beats into the bar pos is, from the beatgrid. Caller holds s.mu.
func (s *sim) barPhase(pos float64) float64 {
	if b := s.tracks[s.cur].Beats; len(b) > 1 {
		i := 0
		for i+2 < len(b) && float64(b[i+1].TimeMS) <= pos {
			i++
		}
		return float64(b[i].Beat) - 1 + (pos-float64(b[i].TimeMS))/float64(b[i+1].TimeMS-b[i].TimeMS)
	}
	return math.Mod(pos/(60000/s.bpm()), 4)
}

// linkBeat is the deck's tempo and bar phase for Link, and whether it follows (BEAT SYNC without MASTER).
func (s *sim) linkBeat() (float64, float64, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bpm() * (1 + s.pitch/100), s.barPhase(s.nowPos()), s.playing, s.syncOn && !s.master
}

// linkSteer feeds the Link session's tempo and bar phase to BEAT SYNC, which
// matches them on the next tick (advance, lockPhase).
func (s *sim) linkSteer(bpm, phase float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sawMasterBeat(bpm, phase, true, time.Now())
}
