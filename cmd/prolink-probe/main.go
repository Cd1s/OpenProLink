// Command prolink-probe records what Pioneer DJ / AlphaTheta Pro DJ Link
// hardware (CDJ and XDJ players, DJM mixers) puts on the wire, for protocol
// work on OpenProLink and EngineX OS.
//
// Players and mixers unicast most of their traffic (CDJ status 0x0a, mixer
// status 0x29, media replies, master handoff) only to devices that announce
// themselves on UDP 50000. So by default the probe listens for 1.5 s, takes a
// free virtual player number (7-15, like beat-link), announces it every second,
// asks each player for its USB/SD media (0x05) and dbserver root menu
// (TCP 12523), and records everything it receives: per packet kind the send
// rate, raw hex samples, a decoded view and a map of every byte offset that
// changed. -passive only listens.
//
// Only the IP and MAC addresses inside packets are rewritten (to 10.0.0.x and
// 02:00:00:00:00:xx); every other byte is kept as received.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/OpenProLink/pkg/cdj"
)

const probeName = "prolink-probe"

type config struct {
	Duration, Prelisten, Interval time.Duration
	Passive                       bool
	Number                        byte // 0 picks a free one
	Samples                       int
	Iface                         string
	// Wire addresses. Tests point them at loopback mocks.
	ListenHost     string
	Ports          [3]int // where we listen for 50000, 50001, 50002
	PeerStatusPort int    // where players take media queries
	DBPort         int
	Announce       *net.UDPAddr // nil: the interface's broadcast address, port 50000
	IP             net.IP
	MAC            net.HardwareAddr
}

func defaultConfig() config {
	return config{Duration: 15 * time.Second, Prelisten: 1500 * time.Millisecond, Interval: time.Second, Samples: 16,
		ListenHost: "0.0.0.0", Ports: [3]int{cdj.PortDiscovery, cdj.PortBeat, cdj.PortStatus},
		PeerStatusPort: cdj.PortStatus, DBPort: cdj.PortDBServer}
}

var logicalPorts = [3]int{cdj.PortDiscovery, cdj.PortBeat, cdj.PortStatus}

type devKey struct {
	ip     [4]byte
	number byte
}

type device struct {
	ip         net.IP
	Name       string `json:"name"`
	Number     byte   `json:"number"`
	IP         string `json:"ip"`
	MAC        string `json:"mac,omitempty"`
	Class      byte   `json:"device_class"`
	Firmware   string `json:"firmware,omitempty"`
	FirstMS    int64  `json:"first_seen_ms"`
	Keepalives int    `json:"keepalives"`
	queried    bool
}

type streamKey struct {
	dev    string
	name   string
	number byte
	port   int
	typ    byte
	sub    byte
	length int
}

type sample struct {
	at  time.Time
	raw []byte
}

type stream struct {
	key                 streamKey
	count               int
	firstAt, lastAt     time.Time
	minGap, maxGap      time.Duration
	first, last, lo, hi []byte
	changes             []int
	seen                [][4]uint64
	firstChg, lastChg   []time.Time
	samples             []sample
	redacted            map[string]bool
	ipLike              map[int]bool
	parsed              any
}

type mediaQuery struct {
	Device     string `json:"device"`
	Number     byte   `json:"number"`
	Slot       string `json:"slot"`
	slot       byte
	ip         [4]byte
	SentMS     int64  `json:"sent_ms"`
	RequestHex string `json:"request_hex"`
	Answered   bool   `json:"answered"`
	ReplyMS    int64  `json:"reply_ms,omitempty"`
}

type probe struct {
	cfg     config
	start   time.Time
	elapsed time.Duration
	conns   [3]*net.UDPConn
	red     redactor
	wg      sync.WaitGroup

	mu       sync.Mutex
	number   byte
	conflict bool
	devices  map[devKey]*device
	streams  map[streamKey]*stream
	media    []*mediaQuery
	db       []*dbReport
	other    map[int]int
	notes    []string
}

func newProbe(cfg config) *probe {
	p := &probe{cfg: cfg, devices: map[devKey]*device{}, streams: map[streamKey]*stream{}, other: map[int]int{},
		red: redactor{ips: map[[4]byte][4]byte{}, macs: map[[6]byte][6]byte{}}}
	if cfg.IP != nil {
		p.red.self(cfg.IP, cfg.MAC)
	}
	return p
}

func (p *probe) ms(t time.Time) int64 { return t.Sub(p.start).Milliseconds() }

func (p *probe) note(format string, a ...any) {
	p.mu.Lock()
	p.notes = append(p.notes, fmt.Sprintf(format, a...))
	p.mu.Unlock()
}

// listen binds the three Pro DJ Link ports, shared with other apps where the OS allows.
func (p *probe) listen(ctx context.Context) error {
	lc := net.ListenConfig{Control: cdj.ReusePort}
	for i, port := range p.cfg.Ports {
		pc, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort(p.cfg.ListenHost, strconv.Itoa(port)))
		if err != nil {
			p.close()
			return fmt.Errorf("listen UDP %d (quit rekordbox and other Pro DJ Link apps): %w", logicalPorts[i], err)
		}
		p.conns[i] = pc.(*net.UDPConn)
	}
	return nil
}

func (p *probe) close() {
	for _, c := range p.conns {
		if c != nil {
			c.Close()
		}
	}
}

// run captures until the duration is over; listen must have succeeded.
func (p *probe) run(ctx context.Context) {
	p.start = time.Now()
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Duration)
	defer cancel()
	var readers sync.WaitGroup
	for i, c := range p.conns {
		readers.Add(1)
		go func() {
			defer readers.Done()
			b := make([]byte, 65535)
			for {
				n, src, err := c.ReadFromUDP(b)
				if err != nil {
					return
				}
				p.handle(logicalPorts[i], src, b[:n], time.Now())
			}
		}()
	}
	if !p.cfg.Passive {
		p.active(ctx)
	}
	<-ctx.Done()
	p.elapsed = time.Since(p.start)
	p.close()
	readers.Wait()
	p.wg.Wait() // media and dbserver queries end on their own deadlines
}

// handle records one datagram. Everything stored is redacted first.
func (p *probe) handle(port int, src *net.UDPAddr, b []byte, at time.Time) {
	if len(b) < 0x0b || !bytes.HasPrefix(b, cdj.MagicHeader) {
		p.mu.Lock()
		p.other[port]++
		p.mu.Unlock()
		return
	}
	typ := b[10]
	nameAt, number, sub := 0x0b, byte(0), byte(0)
	if port == cdj.PortDiscovery {
		nameAt, sub = 0x0c, b[0x0b]
		if typ == cdj.TypeKeepalive && len(b) > 0x24 {
			number = b[0x24]
		}
	} else {
		if len(b) > 0x20 {
			sub = b[0x20]
		}
		if len(b) > 0x21 {
			number = b[0x21]
		}
	}
	name := cstring(b, nameAt, 20)
	var ip [4]byte
	copy(ip[:], src.IP.To4())

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.IP != nil && src.IP.Equal(p.cfg.IP) && name == probeName {
		return // our own broadcast
	}
	p.red.learn(port, typ, ip, b)
	red, where, ipLike := p.red.apply(port, typ, b)

	if port == cdj.PortDiscovery && typ == cdj.TypeKeepalive && len(b) >= 0x36 {
		k := devKey{ip, number}
		d := p.devices[k]
		if d == nil {
			d = &device{ip: net.IP(ip[:]), Name: name, Number: number, IP: p.red.ipString(ip), FirstMS: p.ms(at)}
			p.devices[k] = d
		}
		d.Keepalives++
		d.Class = b[0x34]
		d.MAC = net.HardwareAddr(red[0x26:0x2c]).String()
		if p.number != 0 && number == p.number {
			p.conflict = true
		}
	}
	if port == cdj.PortDiscovery && typ == 0x08 && p.number != 0 && len(b) > 0x24 && b[0x24] == p.number {
		p.conflict = true // a device tells us our number is in use
	}

	k := streamKey{dev: p.red.ipString(ip), name: name, number: number, port: port, typ: typ, sub: sub, length: len(b)}
	s := p.streams[k]
	if s == nil {
		s = &stream{key: k, redacted: map[string]bool{}, ipLike: map[int]bool{}}
		p.streams[k] = s
	}
	s.add(red, at, p.cfg.Samples)
	for _, w := range where {
		s.redacted[w] = true
	}
	for _, o := range ipLike {
		s.ipLike[o] = true
	}
	s.parsed = parse(port, typ, red)

	switch {
	case port == cdj.PortStatus && typ == cdj.TypeStatus:
		if st, ok := s.parsed.(*cdj.Status); ok {
			if d := p.devices[devKey{ip, number}]; d != nil {
				d.Firmware = st.Firmware
			}
		}
	case port == cdj.PortStatus && typ == cdj.TypeMediaResponse:
		if mr, ok := s.parsed.(*cdj.MediaResponse); ok {
			for _, q := range p.media {
				if q.ip == ip && q.Number == mr.PlayerNumber && q.slot == mr.TargetSlot && !q.Answered {
					q.Answered, q.ReplyMS = true, p.ms(at)
				}
			}
		}
	}
}

func (s *stream) add(b []byte, at time.Time, maxSamples int) {
	s.count++
	if s.count == 1 {
		s.firstAt, s.first, s.lo, s.hi = at, clone(b), clone(b), clone(b)
		s.changes, s.seen = make([]int, len(b)), make([][4]uint64, len(b))
		s.firstChg, s.lastChg = make([]time.Time, len(b)), make([]time.Time, len(b))
	} else {
		gap := at.Sub(s.lastAt)
		if s.count == 2 || gap < s.minGap {
			s.minGap = gap
		}
		s.maxGap = max(s.maxGap, gap)
		for i, v := range b {
			if v != s.last[i] {
				s.changes[i]++
				if s.firstChg[i].IsZero() {
					s.firstChg[i] = at
				}
				s.lastChg[i] = at
			}
			s.lo[i], s.hi[i] = min(s.lo[i], v), max(s.hi[i], v)
		}
	}
	for i, v := range b {
		s.seen[i][v>>6] |= 1 << (v & 63)
	}
	s.lastAt, s.last = at, clone(b)
	n := len(s.samples)
	if n == 0 || n < maxSamples && at.Sub(s.samples[n-1].at) >= time.Second && !bytes.Equal(b, s.samples[n-1].raw) {
		s.samples = append(s.samples, sample{at, clone(b)})
	}
}

// active announces a virtual player and queries every player it hears.
func (p *probe) active(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(p.cfg.Prelisten):
	}
	if err := p.pickInterface(); err != nil {
		p.note("not announcing, passive only: %v", err)
		return
	}
	p.mu.Lock()
	n := p.cfg.Number
	if n == 0 {
		n = chooseNumber(p.usedNumbers())
	} else if p.usedNumbers()[n] {
		p.notes = append(p.notes, fmt.Sprintf("-number %d is already on the link", n))
	}
	p.number = n
	p.mu.Unlock()
	if n == 0 {
		p.note("no free player number in 7-15, passive only")
		return
	}
	p.note("announcing as player %d (%s)", n, probeName)
	announced := time.Now()
	tick := time.NewTicker(min(p.cfg.Interval, 250*time.Millisecond))
	defer tick.Stop()
	var lastKeepalive time.Time
	for {
		now := time.Now()
		p.mu.Lock()
		if p.conflict {
			old := p.number
			used := p.usedNumbers()
			used[old] = true
			p.number, p.conflict = chooseNumber(used), false
			p.notes = append(p.notes, fmt.Sprintf("player %d was claimed by another device, switched to %d", old, p.number))
			lastKeepalive = time.Time{}
		}
		number, peers := p.number, len(p.devices)
		var todo []*device
		if now.Sub(announced) >= p.cfg.Interval {
			for _, d := range p.devices {
				if !d.queried && d.Number >= 1 && d.Number <= 6 && !strings.HasPrefix(d.Name, "DJM") {
					d.queried = true
					todo = append(todo, d)
				}
			}
		}
		p.mu.Unlock()
		if number == 0 {
			p.note("no free player number left, stopped announcing")
			return
		}
		if now.Sub(lastKeepalive) >= p.cfg.Interval {
			p.send(0, p.cfg.Announce, cdj.NewKeepalivePacket(cdj.Keepalive{DeviceName: probeName, PlayerNumber: number,
				FirstDeviceFlag: 0x01, MAC: p.cfg.MAC, IP: p.cfg.IP, PeerCount: byte(min(peers+1, 255)),
				DeviceClass: 0x01, ModelCapability: 0x64}))
			lastKeepalive = now
		}
		for _, d := range todo {
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.query(d, number)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *probe) send(i int, to *net.UDPAddr, b []byte) {
	if _, err := p.conns[i].WriteToUDP(b, to); err != nil && !errors.Is(err, net.ErrClosed) {
		p.note("send to UDP %d: %v", to.Port, err)
	}
}

// usedNumbers lists the numbers heard so far. Caller holds p.mu.
func (p *probe) usedNumbers() map[byte]bool {
	used := map[byte]bool{}
	for k := range p.devices {
		used[k.number] = true
	}
	return used
}

// chooseNumber returns the first free number from 7 to 15: 1-6 belong to real
// players (CDJ-3000s go up to 6), 0x11+ to rekordbox, 0x21+ to mixers.
func chooseNumber(used map[byte]bool) byte {
	for n := byte(7); n <= 15; n++ {
		if !used[n] {
			return n
		}
	}
	return 0
}

// pickInterface fills IP, MAC and the broadcast target, preferring the
// interface on the same subnet as the devices heard during the pre-listen.
func (p *probe) pickInterface() error {
	if p.cfg.IP != nil && p.cfg.Announce != nil {
		return nil
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	p.mu.Lock()
	var heard []net.IP
	for _, d := range p.devices {
		heard = append(heard, d.ip)
	}
	p.mu.Unlock()
	type cand struct {
		ip   net.IP
		net  *net.IPNet
		mac  net.HardwareAddr
		name string
	}
	var cands []cand
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 ||
			(p.cfg.Iface != "" && ifc.Name != p.cfg.Iface) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				cands = append(cands, cand{n.IP.To4(), n, ifc.HardwareAddr, ifc.Name})
			}
		}
	}
	if len(cands) == 0 {
		return fmt.Errorf("no IPv4 broadcast interface %s", p.cfg.Iface)
	}
	best := cands[0]
pick:
	for _, c := range cands {
		for _, ip := range heard {
			if c.net.Contains(ip) {
				best = c
				break pick
			}
		}
	}
	bcast := make(net.IP, 4)
	for i := range bcast {
		bcast[i] = best.ip[i] | ^best.net.Mask[len(best.net.Mask)-4+i]
	}
	mac := best.mac
	if len(mac) < 6 {
		mac = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x01}
	}
	p.mu.Lock()
	p.cfg.IP, p.cfg.MAC, p.cfg.Announce = best.ip, mac, &net.UDPAddr{IP: bcast, Port: cdj.PortDiscovery}
	p.red.self(best.ip, mac)
	p.mu.Unlock()
	p.note("using interface %s", best.name)
	return nil
}

// query asks one player for its media (answers arrive on 50002) and its
// dbserver root menu, once per slot.
func (p *probe) query(d *device, me byte) {
	var ip [4]byte
	copy(ip[:], d.ip.To4())
	for _, slot := range []byte{cdj.SlotUSB, cdj.SlotSD} {
		b := cdj.NewMediaQueryPacket(cdj.MediaQuery{DeviceName: probeName, PlayerNumber: me, ReplyIP: p.cfg.IP,
			TargetPlayer: d.Number, TargetSlot: slot})
		p.mu.Lock()
		red, _, _ := p.red.apply(cdj.PortStatus, cdj.TypeMediaQuery, b)
		p.media = append(p.media, &mediaQuery{Device: p.red.ipString(ip), Number: d.Number, Slot: slotName(slot), slot: slot,
			ip: ip, SentMS: p.ms(time.Now()), RequestHex: hex.EncodeToString(red)})
		p.mu.Unlock()
		p.send(2, &net.UDPAddr{IP: d.ip, Port: p.cfg.PeerStatusPort}, b)
	}
	// Players refuse dbserver clients numbered above 4, so pose as the target
	// itself, as beat-link does (ConnectionManager.chooseAskingPlayerNumber).
	asAs := me
	if me > 4 {
		asAs = d.Number
	}
	for _, slot := range []byte{cdj.SlotUSB, cdj.SlotSD} {
		r := dbRoot(net.JoinHostPort(d.ip.String(), strconv.Itoa(p.cfg.DBPort)), asAs, slot)
		p.mu.Lock()
		r.Device, r.Number = p.red.ipString(ip), d.Number
		p.db = append(p.db, r)
		p.mu.Unlock()
	}
}

// dbserver ---------------------------------------------------------------

type dbArg struct {
	Tag  string `json:"tag"`
	Num  uint32 `json:"num,omitempty"`
	Str  string `json:"str,omitempty"`
	Blob string `json:"blob_hex,omitempty"`
}

type dbStep struct {
	Dir  string  `json:"dir"`
	Step string  `json:"step"`
	Hex  string  `json:"hex"`
	Kind string  `json:"kind,omitempty"`
	Tx   *uint32 `json:"tx,omitempty"`
	Args []dbArg `json:"args,omitempty"`
}

type dbItem struct {
	ID        uint32 `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Secondary string `json:"secondary,omitempty"`
}

type dbReport struct {
	Device     string   `json:"device"`
	Number     byte     `json:"number"`
	Slot       string   `json:"slot"`
	AskedAs    byte     `json:"asked_as"`
	Port       int      `json:"dbserver_port,omitempty"`
	RootMenu   []dbItem `json:"root_menu,omitempty"`
	Error      string   `json:"error,omitempty"`
	Transcript []dbStep `json:"transcript"`
}

// tap keeps the raw bytes read since the last take.
type tap struct {
	r   io.Reader
	buf []byte
}

func (t *tap) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	t.buf = append(t.buf, b[:n]...)
	return n, err
}

func (t *tap) take() []byte { b := t.buf; t.buf = nil; return b }

func dbStepOf(dir, step string, raw []byte, m *cdj.DBMessage) dbStep {
	s := dbStep{Dir: dir, Step: step, Hex: hex.EncodeToString(raw)}
	if m != nil {
		tx := m.Tx
		s.Kind, s.Tx = fmt.Sprintf("0x%04x", m.Kind), &tx
		for _, f := range m.Args {
			a := dbArg{Tag: "number", Num: f.Num}
			switch f.Tag {
			case cdj.TagString:
				a = dbArg{Tag: "string", Str: f.Str}
			case cdj.TagBlob:
				a = dbArg{Tag: "blob", Blob: hex.EncodeToString(f.Blob)}
			}
			s.Args = append(s.Args, a)
		}
	}
	return s
}

// dbRoot does what a player does before browsing another player's media:
// port query on 12523, greeting, setup, then the root menu and its render.
func dbRoot(addr string, asAs, slot byte) *dbReport {
	r := &dbReport{Slot: slotName(slot), AskedAs: asAs}
	fail := func(err error) *dbReport { r.Error = err.Error(); return r }
	q, err := net.DialTimeout("tcp4", addr, 3*time.Second)
	if err != nil {
		return fail(err)
	}
	_ = q.SetDeadline(time.Now().Add(3 * time.Second))
	ask := append([]byte{0, 0, 0, 15}, "RemoteDBServer\x00"...)
	r.Transcript = append(r.Transcript, dbStep{Dir: "tx", Step: "port query", Hex: hex.EncodeToString(ask)})
	var port [2]byte
	if _, err = q.Write(ask); err == nil {
		_, err = io.ReadFull(q, port[:])
	}
	q.Close()
	if err != nil {
		return fail(fmt.Errorf("port query: %w", err))
	}
	r.Transcript = append(r.Transcript, dbStep{Dir: "rx", Step: "port query", Hex: hex.EncodeToString(port[:])})
	r.Port = int(binary.BigEndian.Uint16(port[:]))
	host, _, _ := net.SplitHostPort(addr)
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(host, strconv.Itoa(r.Port)), 3*time.Second)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	t := &tap{r: c}
	tx := func(step string, b []byte) error {
		var m *cdj.DBMessage
		if len(b) >= 32 {
			m, _ = cdj.DecodeDBMessageFromBytes(b)
		}
		r.Transcript = append(r.Transcript, dbStepOf("tx", step, b, m))
		_, err := c.Write(b)
		return err
	}
	rx := func(step string) (*cdj.DBMessage, error) {
		m, err := cdj.DecodeDBMessage(t)
		r.Transcript = append(r.Transcript, dbStepOf("rx", step, t.take(), m))
		return m, err
	}
	hello := []byte{0x11, 0, 0, 0, 1}
	if err = tx("greeting", hello); err != nil {
		return fail(err)
	}
	var got [5]byte
	_, err = io.ReadFull(t, got[:])
	r.Transcript = append(r.Transcript, dbStepOf("rx", "greeting", t.take(), nil))
	if err != nil || got != [5]byte(hello) {
		return fail(fmt.Errorf("greeting: %v %x", err, got))
	}
	if err = tx("setup", cdj.EncodeDBRequest(0xfffffffe, cdj.KindSetup, uint32(asAs))); err != nil {
		return fail(err)
	}
	m, err := rx("setup")
	if err != nil {
		return fail(fmt.Errorf("setup: %w", err))
	}
	if m.Kind != cdj.KindSetupAck {
		return fail(fmt.Errorf("setup answered 0x%04x", m.Kind))
	}
	dmst := cdj.BuildDMST(asAs, 1, slot, cdj.TrackTypeRekordbox)
	if err = tx("root menu", cdj.EncodeDBRequest(1, cdj.KindRootMenu, dmst, 0, 0xffffff)); err != nil {
		return fail(err)
	}
	if m, err = rx("root menu"); err != nil {
		return fail(fmt.Errorf("root menu: %w", err))
	}
	if m.Kind != cdj.KindSetupAck || len(m.Args) < 2 {
		return fail(fmt.Errorf("root menu answered 0x%04x (no media in this slot?)", m.Kind))
	}
	count := min(m.Args[1].Num, 64) // a root menu has about a dozen entries
	if count == 0 {
		return r
	}
	if err = tx("render", cdj.EncodeDBRequest(2, cdj.KindRender, dmst, 0, count, 0, count, 0)); err != nil {
		return fail(err)
	}
	if m, err = rx("render header"); err != nil || m.Kind != cdj.KindRenderHeader {
		return fail(fmt.Errorf("render header: %v", errOrKind(err, m)))
	}
	for range count {
		if m, err = rx("render item"); err != nil || m.Kind != cdj.KindRenderItem {
			return fail(fmt.Errorf("render item: %v", errOrKind(err, m)))
		}
		if it, err := cdj.ParseMenuItem(m); err == nil {
			r.RootMenu = append(r.RootMenu, dbItem{ID: it.ID, Kind: fmt.Sprintf("0x%04x", it.Kind), Label: it.Label, Secondary: it.Secondary})
		}
	}
	if m, err = rx("render footer"); err != nil || m.Kind != cdj.KindRenderFooter {
		return fail(fmt.Errorf("render footer: %v", errOrKind(err, m)))
	}
	return r
}

func errOrKind(err error, m *cdj.DBMessage) any {
	if err != nil || m == nil {
		return err
	}
	return fmt.Sprintf("got 0x%04x", m.Kind)
}

// redaction ----------------------------------------------------------------

// redactor maps every real IPv4 and MAC address to a stable stand-in: the
// probe is 10.0.0.1 / 02:00:00:00:00:01, devices count up from .2 / :02.
type redactor struct {
	ips          map[[4]byte][4]byte
	macs         map[[6]byte][6]byte
	nextIP, nMAC int
}

// self maps the probe's own addresses, unless a packet already revealed them
// (rekordbox on the same computer): a mapping never changes mid-capture.
func (r *redactor) self(ip net.IP, mac net.HardwareAddr) {
	var a [4]byte
	copy(a[:], ip.To4())
	if _, ok := r.ips[a]; !ok {
		r.ips[a] = [4]byte{10, 0, 0, 1}
	}
	if len(mac) < 6 {
		return
	}
	if _, ok := r.macs[[6]byte(mac)]; !ok {
		r.macs[[6]byte(mac)] = [6]byte{2, 0, 0, 0, 0, 1}
	}
}

func (r *redactor) ip(a [4]byte) [4]byte {
	if v, ok := r.ips[a]; ok {
		return v
	}
	r.nextIP++
	v := [4]byte{10, 0, byte((r.nextIP + 1) >> 8), byte(r.nextIP + 1)}
	r.ips[a] = v
	return v
}

func (r *redactor) ipString(a [4]byte) string { v := r.ip(a); return net.IP(v[:]).String() }

func (r *redactor) learnMAC(b []byte) {
	if len(b) < 6 {
		return
	}
	m := [6]byte(b[:6])
	if _, ok := r.macs[m]; ok || m == [6]byte{} || m == [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff} {
		return
	}
	r.nMAC++
	r.macs[m] = [6]byte{2, 0, 0, 0, byte((r.nMAC + 1) >> 8), byte(r.nMAC + 1)}
}

// learn records the addresses a packet reveals: its source, and the MAC in
// keepalives (0x26) and number claims (stage 1 at 0x26, stage 2 at 0x28).
func (r *redactor) learn(port int, typ byte, src [4]byte, b []byte) {
	r.ip(src)
	if port != cdj.PortDiscovery {
		return
	}
	switch typ {
	case 0x00, cdj.TypeKeepalive:
		if len(b) >= 0x2c {
			r.learnMAC(b[0x26:])
		}
	case 0x02:
		if len(b) >= 0x2e {
			r.learnMAC(b[0x28:])
		}
	}
}

// apply returns a copy with known addresses replaced. MACs (6 bytes) are
// replaced anywhere; IPs only where the protocol puts them (any offset from
// 0x24 on in port 50000 packets, 0x24 in media queries). Other 4-byte matches
// of a known IP are reported as ipLike, not touched, so no field is ever
// rewritten by coincidence.
func (r *redactor) apply(port int, typ byte, b []byte) (out []byte, where []string, ipLike []int) {
	out = clone(b)
	for real, fake := range r.macs {
		for i := 0x0b; i+6 <= len(out); i++ {
			if [6]byte(out[i:i+6]) == real {
				copy(out[i:], fake[:])
				where = append(where, fmt.Sprintf("0x%02x mac", i))
			}
		}
	}
	for real, fake := range r.ips {
		for i := 0x0b; i+4 <= len(out); i++ {
			if [4]byte(out[i:i+4]) != real {
				continue
			}
			if port == cdj.PortDiscovery && i >= 0x24 || port == cdj.PortStatus && typ == cdj.TypeMediaQuery && i == 0x24 {
				copy(out[i:], fake[:])
				where = append(where, fmt.Sprintf("0x%02x ip", i))
			} else {
				ipLike = append(ipLike, i)
			}
		}
	}
	return out, where, ipLike
}

// decoding -------------------------------------------------------------------

var kinds = map[[2]int]string{
	{cdj.PortDiscovery, 0x00}: "number claim 1", {cdj.PortDiscovery, 0x01}: "number will be assigned",
	{cdj.PortDiscovery, 0x02}: "number claim 2", {cdj.PortDiscovery, 0x03}: "number assignment",
	{cdj.PortDiscovery, 0x04}: "number claim 3", {cdj.PortDiscovery, 0x05}: "number assignment finished",
	{cdj.PortDiscovery, 0x06}: "keepalive", {cdj.PortDiscovery, 0x08}: "number in use", {cdj.PortDiscovery, 0x0a}: "hello",
	{cdj.PortBeat, 0x02}: "fader start", {cdj.PortBeat, 0x03}: "channels on air", {cdj.PortBeat, 0x0b}: "precise position",
	{cdj.PortBeat, 0x26}: "master handoff request", {cdj.PortBeat, 0x27}: "master handoff response",
	{cdj.PortBeat, 0x28}: "beat", {cdj.PortBeat, 0x2a}: "sync control",
	{cdj.PortStatus, 0x05}: "media query", {cdj.PortStatus, 0x06}: "media response", {cdj.PortStatus, 0x0a}: "CDJ status",
	{cdj.PortStatus, 0x10}: "rekordbox lighting hello", {cdj.PortStatus, 0x19}: "load track", {cdj.PortStatus, 0x1a}: "load track ack",
	{cdj.PortStatus, 0x29}: "mixer status", {cdj.PortStatus, 0x34}: "load settings", {cdj.PortStatus, 0x56}: "OPUS metadata",
}

type span struct {
	from, to int
	name     string
}

// fields names the offsets Deep Symmetry's analysis explains, so the change
// map shows at a glance which changed bytes are still unknown. "?" = unverified.
var fields = map[[2]int][]span{
	{cdj.PortDiscovery, 0x06}: {{0x0c, 0x1f, "device name"}, {0x24, 0x24, "device number"}, {0x25, 0x25, "device type?"},
		{0x26, 0x2b, "MAC"}, {0x2c, 0x2f, "IP"}, {0x30, 0x30, "peers seen"}, {0x34, 0x34, "device class?"}, {0x35, 0x35, "capabilities?"}},
	{cdj.PortStatus, 0x0a}: {{0x27, 0x27, "activity"}, {0x28, 0x28, "track source player"}, {0x29, 0x29, "track source slot"},
		{0x2a, 0x2a, "track type"}, {0x2c, 0x2f, "track id"}, {0x32, 0x33, "track number"}, {0x37, 0x37, "disc loaded?"},
		{0x38, 0x3f, "menu path"}, {0x46, 0x47, "track count"}, {0x6f, 0x6f, "USB state"}, {0x73, 0x73, "SD state"},
		{0x75, 0x75, "link available"}, {0x7b, 0x7b, "play state 1"}, {0x7c, 0x7f, "firmware"},
		{0x84, 0x87, "sync counter"}, {0x89, 0x89, "flags: play 40 master 20 sync 10 on-air 08 bpm-sync 02"},
		{0x8b, 0x8b, "play state 2"}, {0x8c, 0x8f, "pitch 1"}, {0x90, 0x91, "BPM valid"}, {0x92, 0x93, "BPM x100"},
		{0x98, 0x9b, "pitch 2"}, {0x9d, 0x9d, "play state 3"}, {0x9e, 0x9e, "master meaningful"}, {0x9f, 0x9f, "master handoff"},
		{0xa0, 0xa3, "beat number"}, {0xa4, 0xa5, "cue countdown"}, {0xa6, 0xa6, "beat in bar"}, {0xb7, 0xb7, "media presence"},
		{0xb8, 0xb8, "USB unsafe eject"}, {0xb9, 0xb9, "SD unsafe eject"}, {0xba, 0xba, "emergency loop"},
		{0xc0, 0xc3, "pitch 3"}, {0xc4, 0xc7, "pitch 4"}, {0xcc, 0xcc, "player generation?"}, {0xcd, 0xcd, "touch audio (bit 5)?"},
		{0xda, 0xda, "waveform colour"}, {0x11f, 0x11f, "buffer full?"}, {0x158, 0x158, "master tempo?"},
		{0x1b6, 0x1b9, "loop start"}, {0x1be, 0x1c1, "loop end"}, {0x1c8, 0x1c9, "loop beats"}},
	{cdj.PortStatus, 0x29}: {{0x27, 0x27, "flags: master f0 / not d0"}, {0x28, 0x2b, "pitch"}, {0x2e, 0x2f, "BPM x100"},
		{0x30, 0x33, "pitch 2?"}, {0x36, 0x36, "master handoff"}, {0x37, 0x37, "beat in bar"}},
	{cdj.PortStatus, 0x06}: {{0x27, 0x27, "player"}, {0x2b, 0x2b, "slot"}, {0x2c, 0x6b, "media name"}, {0x6c, 0x87, "created"},
		{0xa6, 0xa7, "track count"}, {0xa8, 0xa8, "colour"}, {0xaa, 0xaa, "track type"}, {0xab, 0xab, "has settings"},
		{0xae, 0xaf, "playlist count"}, {0xb0, 0xb7, "total bytes"}, {0xb8, 0xbf, "free bytes"}},
	{cdj.PortBeat, 0x28}: {{0x24, 0x27, "next beat ms"}, {0x28, 0x2b, "2nd beat ms"}, {0x2c, 0x2f, "next bar ms"},
		{0x30, 0x33, "4th beat ms"}, {0x34, 0x37, "2nd bar ms"}, {0x38, 0x3b, "8th beat ms"}, {0x54, 0x57, "pitch"},
		{0x5a, 0x5b, "BPM x100"}, {0x5c, 0x5c, "beat in bar"}},
	{cdj.PortBeat, 0x0b}: {{0x24, 0x27, "track length s"}, {0x28, 0x2b, "playhead ms"}, {0x2c, 0x2f, "pitch x100"},
		{0x38, 0x3b, "effective BPM x10"}},
	{cdj.PortBeat, 0x03}: {{0x24, 0x24, "ch1 on air"}, {0x25, 0x25, "ch2 on air"}, {0x26, 0x26, "ch3 on air"},
		{0x27, 0x27, "ch4 on air"}, {0x2d, 0x2d, "ch5 on air"}, {0x2e, 0x2e, "ch6 on air"}},
	{cdj.PortBeat, 0x2a}: {{0x27, 0x27, "from player"}, {0x2b, 0x2b, "sync: on 10 / off 20"}},
}

func fieldName(port int, typ byte, off int) string {
	if off < 0x0b {
		return "header"
	}
	for _, f := range fields[[2]int{port, int(typ)}] {
		if off >= f.from && off <= f.to {
			return f.name
		}
	}
	switch {
	case port != cdj.PortDiscovery && off < 0x1f, port == cdj.PortDiscovery && off >= 0x0c && off < 0x20:
		return "device name"
	case port != cdj.PortDiscovery && off == 0x21:
		return "device number"
	case off == 0x22 || off == 0x23:
		return "length"
	}
	return ""
}

type mixerStatus struct {
	Flags         string  `json:"flags"`
	Master        bool    `json:"master"`
	Sync          bool    `json:"sync"`
	Playing       bool    `json:"playing"`
	Pitch         float64 `json:"pitch"`
	BPM           float64 `json:"bpm"`
	MasterHandoff byte    `json:"master_handoff"`
	BeatInBar     byte    `json:"beat_in_bar"`
}

type keepalive struct {
	Name     string `json:"name"`
	Number   byte   `json:"number"`
	Type     byte   `json:"device_type"`
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Peers    byte   `json:"peers"`
	Class    byte   `json:"device_class"`
	Byte0x35 byte   `json:"byte_0x35"`
}

func parse(port int, typ byte, b []byte) any {
	var v any
	var err error
	switch [2]int{port, int(typ)} {
	case [2]int{cdj.PortDiscovery, 0x06}:
		var k *cdj.Keepalive
		if k, err = cdj.ParseKeepalivePacket(b); err == nil {
			v = keepalive{k.DeviceName, k.PlayerNumber, k.FirstDeviceFlag, k.MAC.String(), k.IP.String(), k.PeerCount, k.DeviceClass, k.ModelCapability}
		}
	case [2]int{cdj.PortStatus, 0x0a}:
		v, err = cdj.ParseStatusPacket(b)
	case [2]int{cdj.PortStatus, 0x05}:
		v, err = cdj.ParseMediaQueryPacket(b)
	case [2]int{cdj.PortStatus, 0x06}:
		v, err = cdj.ParseMediaResponsePacket(b)
	case [2]int{cdj.PortBeat, 0x28}:
		v, err = cdj.ParseBeatPacket(b)
	case [2]int{cdj.PortBeat, 0x0b}:
		v, err = cdj.ParseAbsolutePositionPacket(b)
	case [2]int{cdj.PortStatus, 0x29}:
		if len(b) < 0x38 {
			return map[string]string{"parse_error": "mixer status shorter than 0x38"}
		}
		f := b[0x27]
		v = mixerStatus{fmt.Sprintf("0x%02x", f), f&cdj.FlagMaster != 0, f&cdj.FlagSync != 0, f&cdj.FlagPlay != 0,
			cdj.WordToPitch(binary.BigEndian.Uint32(b[0x28:])), float64(binary.BigEndian.Uint16(b[0x2e:])) / 100, b[0x36], b[0x37]}
	case [2]int{cdj.PortBeat, 0x03}:
		if len(b) < 0x28 {
			return map[string]string{"parse_error": "on-air packet shorter than 0x28"}
		}
		ch := []bool{b[0x24] != 0, b[0x25] != 0, b[0x26] != 0, b[0x27] != 0}
		if len(b) >= 0x2f {
			ch = append(ch, b[0x2d] != 0, b[0x2e] != 0)
		}
		v = map[string][]bool{"on_air": ch}
	default:
		return nil
	}
	if err != nil {
		return map[string]string{"parse_error": err.Error()}
	}
	return v
}

// report -----------------------------------------------------------------------

type gapOut struct {
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

type changeOut struct {
	Offset   string   `json:"offset"`
	Field    string   `json:"field,omitempty"`
	First    string   `json:"first"`
	Last     string   `json:"last"`
	Min      string   `json:"min"`
	Max      string   `json:"max"`
	Changes  int      `json:"changes"`
	Distinct int      `json:"distinct"`
	Values   []string `json:"values"`
	FirstMS  int64    `json:"first_change_ms"`
	LastMS   int64    `json:"last_change_ms"`
}

type sampleOut struct {
	MS  int64  `json:"t_ms"`
	Hex string `json:"hex"`
}

type streamOut struct {
	Device   string      `json:"device"`
	Name     string      `json:"name"`
	Number   byte        `json:"number"`
	Port     int         `json:"port"`
	Type     string      `json:"type"`
	Kind     string      `json:"kind"`
	Subtype  string      `json:"subtype"`
	Length   int         `json:"length"`
	Count    int         `json:"count"`
	Interval *gapOut     `json:"interval_ms,omitempty"`
	Redacted []string    `json:"redacted,omitempty"`
	IPLike   []string    `json:"unredacted_ip_like,omitempty"`
	Parsed   any         `json:"parsed,omitempty"`
	Changed  []changeOut `json:"changed_bytes"`
	Samples  []sampleOut `json:"samples"`
}

type report struct {
	Tool         string         `json:"tool"`
	Format       int            `json:"format"`
	Started      string         `json:"started"`
	DurationMS   int64          `json:"duration_ms"`
	Mode         string         `json:"mode"`
	OS           string         `json:"os"`
	Probe        any            `json:"probe,omitempty"`
	Devices      []*device      `json:"devices"`
	Streams      []streamOut    `json:"streams"`
	MediaQueries []*mediaQuery  `json:"media_queries,omitempty"`
	DBServer     []*dbReport    `json:"dbserver,omitempty"`
	NonProLink   map[string]int `json:"non_prolink_packets,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
}

func (p *probe) report() report {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := report{Tool: probeName, Format: 1, Started: p.start.UTC().Format(time.RFC3339), DurationMS: p.elapsed.Milliseconds(),
		Mode: "active", OS: runtime.GOOS, Devices: []*device{}, Streams: []streamOut{}, MediaQueries: p.media, DBServer: p.db,
		Notes: append([]string(nil), p.notes...)}
	if p.cfg.Passive {
		r.Mode = "passive"
	} else if p.number != 0 {
		mac := p.red.macs[[6]byte(p.cfg.MAC)]
		r.Probe = map[string]any{"name": probeName, "number": p.number, "ip": p.red.ipString([4]byte(p.cfg.IP.To4())),
			"mac": net.HardwareAddr(mac[:]).String()}
	}
	for _, d := range p.devices {
		r.Devices = append(r.Devices, d)
	}
	sort.Slice(r.Devices, func(i, j int) bool {
		a, b := r.Devices[i], r.Devices[j]
		return a.IP < b.IP || a.IP == b.IP && a.Number < b.Number
	})
	for _, s := range p.streams {
		r.Streams = append(r.Streams, s.out(p))
	}
	sort.Slice(r.Streams, func(i, j int) bool {
		a, b := r.Streams[i], r.Streams[j]
		return fmt.Sprintf("%s/%03d/%05d/%s/%s/%05d", a.Device, a.Number, a.Port, a.Type, a.Subtype, a.Length) <
			fmt.Sprintf("%s/%03d/%05d/%s/%s/%05d", b.Device, b.Number, b.Port, b.Type, b.Subtype, b.Length)
	})
	for port, n := range p.other {
		if r.NonProLink == nil {
			r.NonProLink = map[string]int{}
		}
		r.NonProLink[strconv.Itoa(port)] = n
	}
	if len(p.streams) == 0 {
		r.Notes = append(r.Notes, "no Pro DJ Link packets heard: check the cable or switch, that this computer is on the devices' subnet, and the firewall")
	}
	return r
}

func (s *stream) out(p *probe) streamOut {
	k := s.key
	o := streamOut{Device: k.dev, Name: k.name, Number: k.number, Port: k.port, Type: fmt.Sprintf("0x%02x", k.typ),
		Kind: kinds[[2]int{k.port, int(k.typ)}], Subtype: fmt.Sprintf("0x%02x", k.sub), Length: k.length, Count: s.count,
		Parsed: s.parsed, Changed: []changeOut{}}
	if s.count > 1 {
		ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
		o.Interval = &gapOut{Avg: ms(s.lastAt.Sub(s.firstAt) / time.Duration(s.count-1)), Min: ms(s.minGap), Max: ms(s.maxGap)}
	}
	for w := range s.redacted {
		o.Redacted = append(o.Redacted, w)
	}
	sort.Strings(o.Redacted)
	for off := range s.ipLike {
		o.IPLike = append(o.IPLike, fmt.Sprintf("0x%02x", off))
	}
	sort.Strings(o.IPLike)
	for i, n := range s.changes {
		if n == 0 {
			continue
		}
		c := changeOut{Offset: fmt.Sprintf("0x%02x", i), Field: fieldName(k.port, k.typ, i), First: hex2(s.first[i]),
			Last: hex2(s.last[i]), Min: hex2(s.lo[i]), Max: hex2(s.hi[i]), Changes: n, FirstMS: p.ms(s.firstChg[i]), LastMS: p.ms(s.lastChg[i])}
		for w, word := range s.seen[i] {
			c.Distinct += bits.OnesCount64(word)
			for bit := range 64 {
				if word&(1<<bit) != 0 && len(c.Values) < 16 {
					c.Values = append(c.Values, hex2(byte(w*64+bit)))
				}
			}
		}
		o.Changed = append(o.Changed, c)
	}
	for _, sm := range s.samples {
		o.Samples = append(o.Samples, sampleOut{p.ms(sm.at), hex.EncodeToString(sm.raw)})
	}
	if last := s.samples[len(s.samples)-1]; !bytes.Equal(last.raw, s.last) {
		o.Samples = append(o.Samples, sampleOut{p.ms(s.lastAt), hex.EncodeToString(s.last)})
	}
	return o
}

// summary prints one line per device and per packet kind.
func (r report) summary(w io.Writer) {
	fmt.Fprintf(w, "%s: %d devices, %d packet kinds in %s (%s)\n", r.Tool, len(r.Devices), len(r.Streams),
		time.Duration(r.DurationMS)*time.Millisecond, r.Mode)
	for _, d := range r.Devices {
		fmt.Fprintf(w, "  device %-9s #%-3d %-14s %s\n", d.IP, d.Number, d.Name, d.Firmware)
	}
	for _, s := range r.Streams {
		every := ""
		if s.Interval != nil {
			every = fmt.Sprintf("every %.0f ms", s.Interval.Avg)
		}
		fmt.Fprintf(w, "  %-9s #%-3d %d %s %-24s len %3d  %5d pkts %-16s %d bytes changed\n",
			s.Device, s.Number, s.Port, s.Type, s.Kind, s.Length, s.Count, every, len(s.Changed))
	}
	for _, q := range r.MediaQueries {
		fmt.Fprintf(w, "  media query #%d %s answered=%v\n", q.Number, q.Slot, q.Answered)
	}
	for _, d := range r.DBServer {
		fmt.Fprintf(w, "  dbserver #%d %s: %d root menu items %s\n", d.Number, d.Slot, len(d.RootMenu), d.Error)
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, "  note:", n)
	}
}

func slotName(s byte) string {
	switch s {
	case cdj.SlotUSB:
		return "usb"
	case cdj.SlotSD:
		return "sd"
	}
	return fmt.Sprintf("0x%02x", s)
}

func cstring(b []byte, at, n int) string {
	if at >= len(b) {
		return ""
	}
	s := b[at:min(len(b), at+n)]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(string(s))
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }
func hex2(v byte) string    { return fmt.Sprintf("0x%02x", v) }

func run() error {
	cfg := defaultConfig()
	flag.DurationVar(&cfg.Duration, "duration", cfg.Duration, "capture duration (60s gives time to work the controls)")
	out := flag.String("out", "capture.json", "output JSON report")
	jsonOut := flag.String("json", "", "write JSON samples here and a Markdown summary to -out")
	flag.BoolVar(&cfg.Passive, "passive", false, "only listen: do not announce a virtual player or query devices (players then send far less)")
	number := flag.Int("number", 0, "virtual player number to announce (default: first free from 7 to 15)")
	flag.StringVar(&cfg.Iface, "iface", "", "network interface to announce on (default: the one on the devices' subnet)")
	flag.IntVar(&cfg.Samples, "samples", cfg.Samples, "raw hex samples kept per packet kind (at most one per second)")
	flag.Parse()
	if *jsonOut != "" && *jsonOut == *out {
		return fmt.Errorf("-out and -json must name different files")
	}
	if cfg.Duration <= cfg.Prelisten && !cfg.Passive || cfg.Duration <= 0 {
		return fmt.Errorf("-duration must be longer than %s", cfg.Prelisten)
	}
	if *number < 0 || *number > 0x7f || cfg.Samples < 1 {
		return fmt.Errorf("-number must be 1-127 and -samples at least 1")
	}
	cfg.Number = byte(*number)
	p := newProbe(cfg)
	if err := p.listen(context.Background()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: capturing for %s (Ctrl-C stops early and still writes the report)\n", probeName, cfg.Duration)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p.run(ctx)
	r := p.report()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := *out
	if *jsonOut != "" {
		jsonPath = *jsonOut
	}
	if err := os.WriteFile(jsonPath, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if *jsonOut != "" {
		var summary bytes.Buffer
		summary.WriteString("# Pro DJ Link capture\n\n```text\n")
		r.summary(&summary)
		summary.WriteString("```\n\nReview the JSON samples for private data before sharing.\n")
		if err := os.WriteFile(*out, summary.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s; review it before attaching it to an issue\n", jsonPath)
	}
	r.summary(os.Stderr)
	fmt.Fprintf(os.Stderr, "wrote %s; review it before attaching it to an issue\n", *out)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, probeName+":", err)
		os.Exit(1)
	}
}
