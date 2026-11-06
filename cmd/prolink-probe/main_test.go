package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/OpenProLink/pkg/cdj"
)

var (
	loop        = net.IPv4(127, 0, 0, 1).To4()
	probeMAC    = net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	cdjMAC      = net.HardwareAddr{0xc8, 0x3d, 0xfc, 0x01, 0x02, 0x03}
	djmMAC      = net.HardwareAddr{0xc8, 0x3d, 0xfc, 0x04, 0x05, 0x06}
	squatterMAC = net.HardwareAddr{0xc8, 0x3d, 0xfc, 0x09, 0x09, 0x09}
)

func TestChooseNumber(t *testing.T) {
	if n := chooseNumber(map[byte]bool{1: true, 2: true, 7: true, 33: true}); n != 8 {
		t.Fatalf("chooseNumber = %d, want 8", n)
	}
	full := map[byte]bool{}
	for n := byte(7); n <= 15; n++ {
		full[n] = true
	}
	if n := chooseNumber(full); n != 0 {
		t.Fatalf("chooseNumber(all taken) = %d, want 0", n)
	}
}

// Addresses are rewritten only where the protocol carries them; a status
// field that happens to hold the same four bytes as an IP is left alone.
func TestRedactorTouchesOnlyAddresses(t *testing.T) {
	r := redactor{ips: map[[4]byte][4]byte{}, macs: map[[6]byte][6]byte{}}
	r.self(loop, probeMAC)
	ka := cdj.NewKeepalivePacket(cdj.Keepalive{DeviceName: "CDJ-3000", PlayerNumber: 2, MAC: cdjMAC, IP: net.IPv4(192, 168, 1, 20)})
	r.learn(cdj.PortDiscovery, cdj.TypeKeepalive, [4]byte{192, 168, 1, 20}, ka)
	out, where, _ := r.apply(cdj.PortDiscovery, cdj.TypeKeepalive, ka)
	if !slices.Equal(where, []string{"0x26 mac", "0x2c ip"}) && !slices.Equal(where, []string{"0x2c ip", "0x26 mac"}) {
		t.Fatalf("redacted %v", where)
	}
	if !bytes.Equal(out[0x26:0x30], []byte{2, 0, 0, 0, 0, 2, 10, 0, 0, 2}) {
		t.Fatalf("keepalive addresses % x", out[0x26:0x30])
	}
	if !bytes.Equal(out[:0x26], ka[:0x26]) || !bytes.Equal(out[0x30:], ka[0x30:]) {
		t.Fatal("keepalive bytes outside the addresses changed")
	}
	st := cdj.NewCDJ3000StatusPacket(cdj.Status{PlayerNumber: 2, TrackID: binary.BigEndian.Uint32([]byte{192, 168, 1, 20}), BPM: 128})
	out, where, ipLike := r.apply(cdj.PortStatus, cdj.TypeStatus, st)
	if !bytes.Equal(out, st) || len(where) != 0 || !slices.Equal(ipLike, []int{0x2c}) {
		t.Fatalf("status packet changed: where %v ipLike %v", where, ipLike)
	}
}

// bench runs the probe against loopback mocks of a CDJ-3000 (player 2), a
// DJM-V10 (0x21) and a squatter that takes the probe's number. Like real
// hardware, the mocks unicast status only once the probe has announced itself.
type bench struct {
	t          *testing.T
	p          *probe
	ports      [3]int
	registered chan struct{}
	once       sync.Once
	stop       chan struct{}
	wg         sync.WaitGroup
	cdjOut     *net.UDPConn
	djmOut     *net.UDPConn
	sqOut      *net.UDPConn

	mu        sync.Mutex
	announced []byte // probe numbers seen in its keepalives
	keepalive []byte // the probe's first keepalive
	setupArgs []uint32
}

func udp(t *testing.T) *net.UDPConn {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loop})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func port(a net.Addr) int { return a.(*net.UDPAddr).Port }

func newBench(t *testing.T, passive bool) *bench {
	b := &bench{t: t, registered: make(chan struct{}), stop: make(chan struct{})}
	registrar, cdjStatus := udp(t), udp(t)
	b.cdjOut, b.djmOut, b.sqOut = udp(t), udp(t), udp(t)
	dbPort := b.dbserver()
	cfg := defaultConfig()
	cfg.ListenHost, cfg.Ports = "127.0.0.1", [3]int{}
	cfg.Duration, cfg.Prelisten, cfg.Interval = 3*time.Second, 300*time.Millisecond, 200*time.Millisecond
	cfg.Passive, cfg.IP, cfg.MAC = passive, loop, probeMAC
	cfg.Announce = registrar.LocalAddr().(*net.UDPAddr)
	cfg.PeerStatusPort, cfg.DBPort = port(cdjStatus.LocalAddr()), dbPort
	b.p = newProbe(cfg)
	if err := b.p.listen(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, c := range b.p.conns {
		b.ports[i] = port(c.LocalAddr())
	}
	b.go_(func() { b.registrar(registrar) })
	b.go_(func() { b.mediaReplies(cdjStatus) })
	b.go_(b.cdj)
	b.go_(b.djm)
	b.go_(b.squatter)
	return b
}

func (b *bench) go_(f func()) {
	b.wg.Add(1)
	go func() { defer b.wg.Done(); f() }()
}

func (b *bench) run() report {
	b.p.run(context.Background())
	close(b.stop)
	b.wg.Wait()
	return b.p.report()
}

func (b *bench) to(i int) *net.UDPAddr { return &net.UDPAddr{IP: loop, Port: b.ports[i]} }

func (b *bench) isRegistered() bool {
	select {
	case <-b.registered:
		return true
	default:
		return false
	}
}

// every calls f each period until the bench stops.
func (b *bench) every(period time.Duration, f func(n int)) {
	t := time.NewTicker(period)
	defer t.Stop()
	for n := 0; ; n++ {
		f(n)
		select {
		case <-b.stop:
			return
		case <-t.C:
		}
	}
}

func (b *bench) registrar(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := c.ReadFromUDP(buf)
		select {
		case <-b.stop:
			return
		default:
		}
		if err != nil {
			continue
		}
		k, err := cdj.ParseKeepalivePacket(buf[:n])
		if err != nil {
			b.t.Errorf("probe sent a bad keepalive: %v", err)
			continue
		}
		b.mu.Lock()
		if !slices.Contains(b.announced, k.PlayerNumber) {
			b.announced = append(b.announced, k.PlayerNumber)
		}
		if b.keepalive == nil {
			b.keepalive = clone(buf[:n])
		}
		b.mu.Unlock()
		b.once.Do(func() { close(b.registered) })
	}
}

func (b *bench) cdj() {
	out := b.cdjOut
	ka := cdj.NewKeepalivePacket(cdj.Keepalive{DeviceName: "CDJ-3000", PlayerNumber: 2, MAC: cdjMAC, IP: loop, DeviceClass: 1, ModelCapability: 0x64})
	b.every(10*time.Millisecond, func(n int) {
		if n%10 == 0 {
			out.WriteToUDP(ka, b.to(0))
		}
		if !b.isRegistered() {
			return
		}
		pitch := float64(n/50) * 0.5
		if n%3 == 0 {
			out.WriteToUDP(cdj.NewAbsolutePositionPacket(cdj.AbsolutePosition{DeviceName: "CDJ-3000", PlayerNumber: 2,
				TrackLengthSec: 300, PlayheadMS: uint32(n * 10), HasPlayhead: true, Pitch: pitch, EffectiveBPM: 124 * (1 + pitch/100)}), b.to(1))
		}
		if n%4 == 0 {
			out.WriteToUDP(cdj.NewCDJ3000StatusPacket(cdj.Status{DeviceName: "CDJ-3000", PlayerNumber: 2, TrackSourcePlayer: 2,
				TrackSourceSlot: cdj.SlotUSB, TrackType: cdj.TrackTypeRekordbox, TrackID: 5, Firmware: "3.20", Playing: n/30%2 == 0,
				Master: true, Pitch: pitch, BPM: 124, Beat: uint32(n / 48), BeatWithinBar: byte(n/48%4 + 1)}), b.to(2))
		}
		if n%48 == 0 {
			out.WriteToUDP(cdj.NewBeatPacket(cdj.Beat{DeviceName: "CDJ-3000", PlayerNumber: 2, NextBeatMS: 484, BPM: 124,
				Pitch: pitch, BeatWithinBar: byte(n/48%4 + 1)}), b.to(1))
		}
	})
}

func header(typ byte, name string, sub, number byte, size int) []byte {
	p := make([]byte, size)
	copy(p, cdj.MagicHeader)
	p[10] = typ
	copy(p[0x0b:], name)
	p[0x1f], p[0x20], p[0x21] = 0x01, sub, number
	binary.BigEndian.PutUint16(p[0x22:], uint16(size-0x24))
	return p
}

func (b *bench) djm() {
	out := b.djmOut
	ka := cdj.NewKeepalivePacket(cdj.Keepalive{DeviceName: "DJM-V10", PlayerNumber: 0x21, MAC: djmMAC, IP: loop, DeviceClass: 2})
	b.every(10*time.Millisecond, func(n int) {
		if n%10 == 0 {
			out.WriteToUDP(ka, b.to(0))
		}
		if !b.isRegistered() {
			return
		}
		if n%4 == 0 { // mixer status, master on and off every 300 ms
			p := header(0x29, "DJM-V10", 0x00, 0x21, 0x38)
			p[0x24], p[0x27] = 0x21, 0xf0
			if n/30%2 == 1 {
				p[0x27] = 0xd0
			}
			binary.BigEndian.PutUint32(p[0x28:], 0x100000)
			binary.BigEndian.PutUint16(p[0x2e:], 12800)
			binary.BigEndian.PutUint32(p[0x30:], 0x100000)
			p[0x36], p[0x37] = 0xff, byte(n/47%4+1)
			out.WriteToUDP(p, b.to(2))
		}
		if n%5 == 0 { // six-channel on-air, channel 2 fader up and down every 250 ms
			p := header(0x03, "DJM-V10", 0x03, 0x21, 0x35)
			p[0x24], p[0x25] = 1, byte(n/25%2)
			out.WriteToUDP(p, b.to(1))
		}
		if n%47 == 0 {
			out.WriteToUDP(cdj.NewBeatPacket(cdj.Beat{DeviceName: "DJM-V10", PlayerNumber: 0x21, NextBeatMS: 469, BPM: 128,
				BeatWithinBar: byte(n/47%4 + 1)}), b.to(1))
		}
	})
}

// squatter claims the probe's first number 400 ms after the probe announced it.
func (b *bench) squatter() {
	select {
	case <-b.registered:
	case <-b.stop:
		return
	}
	time.Sleep(400 * time.Millisecond)
	b.mu.Lock()
	n := b.announced[0]
	b.mu.Unlock()
	out := b.sqOut
	ka := cdj.NewKeepalivePacket(cdj.Keepalive{DeviceName: "CDJ-3000", PlayerNumber: n, MAC: squatterMAC, IP: loop})
	for range 3 {
		out.WriteToUDP(ka, b.to(0))
		time.Sleep(50 * time.Millisecond)
	}
}

// mediaReplies answers the USB slot only, like a player with no SD card.
func (b *bench) mediaReplies(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := c.ReadFromUDP(buf)
		select {
		case <-b.stop:
			return
		default:
		}
		if err != nil {
			continue
		}
		q, err := cdj.ParseMediaQueryPacket(buf[:n])
		if err != nil || q.TargetSlot != cdj.SlotUSB {
			continue
		}
		c.WriteToUDP(cdj.NewMediaResponsePacket(cdj.MediaResponse{DeviceName: "CDJ-3000", PlayerNumber: 2, TargetPlayer: 2,
			TargetSlot: q.TargetSlot, MediaName: "PROBE USB", TrackCount: 42, PlaylistCount: 3}), &net.UDPAddr{IP: q.ReplyIP, Port: b.ports[2]})
	}
}

// dbserver is a player's port query on one listener and its dbserver on
// another: a root menu of three items for USB, "unavailable" for SD.
func (b *bench) dbserver() int {
	lq, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.t.Fatal(err)
	}
	ldb, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { lq.Close(); ldb.Close() })
	go func() {
		for {
			c, err := lq.Accept()
			if err != nil {
				return
			}
			var ask [19]byte
			if _, err := io.ReadFull(c, ask[:]); err == nil && string(ask[4:]) == "RemoteDBServer\x00" {
				c.Write(binary.BigEndian.AppendUint16(nil, uint16(ldb.Addr().(*net.TCPAddr).Port)))
			}
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := ldb.Accept()
			if err != nil {
				return
			}
			go b.dbSession(c)
		}
	}()
	return lq.Addr().(*net.TCPAddr).Port
}

func (b *bench) dbSession(c net.Conn) {
	defer c.Close()
	var hello [5]byte
	if _, err := io.ReadFull(c, hello[:]); err != nil {
		return
	}
	c.Write(hello[:])
	num := func(v uint32) cdj.DBField { return cdj.DBField{Tag: cdj.TagNumber, Num: v} }
	str := func(s string) cdj.DBField { return cdj.DBField{Tag: cdj.TagString, Str: s} }
	send := func(tx uint32, kind uint16, args ...cdj.DBField) {
		c.Write(cdj.EncodeDBMessage(cdj.DBMessage{Tx: tx, Kind: kind, Args: args}))
	}
	labels := []string{"ARTIST", "ALBUM", "TRACK"}
	for {
		m, err := cdj.DecodeDBMessage(c)
		if err != nil {
			return
		}
		switch m.Kind {
		case cdj.KindSetup:
			b.mu.Lock()
			b.setupArgs = append(b.setupArgs, m.Args[0].Num)
			b.mu.Unlock()
			send(m.Tx, cdj.KindSetupAck, num(0), num(2))
		case cdj.KindRootMenu:
			if byte(m.Args[0].Num>>8) != cdj.SlotUSB {
				send(m.Tx, cdj.KindUnavailable, num(0))
				continue
			}
			send(m.Tx, cdj.KindSetupAck, num(uint32(cdj.KindRootMenu)), num(uint32(len(labels))))
		case cdj.KindRender:
			send(m.Tx, cdj.KindRenderHeader, num(1), num(0))
			for i, l := range labels {
				send(m.Tx, cdj.KindRenderItem, num(0), num(uint32(i+1)), num(uint32(len(l)+3)), str("￺"+l+"￻"),
					num(2), str(""), num(0x81+uint32(i)), num(0), num(0), num(uint32(i)))
			}
			send(m.Tx, cdj.KindRenderFooter)
		}
	}
}

func find(r report, port int, typ byte, name string) *streamOut {
	for i, s := range r.Streams {
		if s.Port == port && s.Type == hex2(typ) && s.Name == name {
			return &r.Streams[i]
		}
	}
	return nil
}

func changed(s *streamOut, off string) *changeOut {
	for i, c := range s.Changed {
		if c.Offset == off {
			return &s.Changed[i]
		}
	}
	return nil
}

func TestProbeCapturesCDJ3000AndDJM(t *testing.T) {
	b := newBench(t, false)
	r := b.run()

	b.mu.Lock()
	announced, ka, setup := b.announced, b.keepalive, b.setupArgs
	b.mu.Unlock()
	// Announced as 7 (2 and 0x21 were taken), then yielded 7 to the squatter.
	if !slices.Equal(announced, []byte{7, 8}) {
		t.Fatalf("probe announced numbers %v, want [7 8]", announced)
	}
	k, _ := cdj.ParseKeepalivePacket(ka)
	if k.DeviceName != probeName || k.MAC.String() != probeMAC.String() || !k.IP.Equal(loop) || k.ModelCapability != 0x64 {
		t.Fatalf("probe keepalive %+v", k)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "player 7 was claimed by another device, switched to 8") {
		t.Fatalf("notes %q", r.Notes)
	}

	st := find(r, cdj.PortStatus, cdj.TypeStatus, "CDJ-3000")
	if st == nil || st.Count < 20 || st.Length != 0x200 || st.Interval == nil || st.Interval.Avg < 20 || st.Interval.Avg > 150 {
		t.Fatalf("CDJ status stream %+v", st)
	}
	if s, ok := st.Parsed.(*cdj.Status); !ok || s.Firmware != "3.20" || s.TrackID != 5 || s.BPM != 124 {
		t.Fatalf("CDJ status parsed %+v", st.Parsed)
	}
	if c := changed(st, "0x89"); c == nil || !strings.HasPrefix(c.Field, "flags") || !slices.Contains(c.Values, "0xe4") || !slices.Contains(c.Values, "0xa4") {
		t.Fatalf("play flag change not tracked: %+v", c)
	}
	if c := changed(st, "0x8e"); c == nil || c.Field != "pitch 1" {
		t.Fatalf("pitch change not tracked: %+v", c)
	}
	if len(st.Redacted) != 0 || len(st.Samples) == 0 || len(st.Samples[0].Hex) != 0x400 {
		t.Fatalf("status samples %v redacted %v", len(st.Samples), st.Redacted)
	}
	for _, typ := range []byte{cdj.TypeAbsolutePosition, cdj.TypeBeat} {
		if s := find(r, cdj.PortBeat, typ, "CDJ-3000"); s == nil || s.Count < 3 || s.Parsed == nil {
			t.Fatalf("CDJ-3000 50001 0x%02x stream %+v", typ, s)
		}
	}

	mx := find(r, cdj.PortStatus, 0x29, "DJM-V10")
	if mx == nil || mx.Number != 0x21 || mx.Kind != "mixer status" || mx.Count < 20 {
		t.Fatalf("mixer status stream %+v", mx)
	}
	if c := changed(mx, "0x27"); c == nil || !slices.Equal(c.Values, []string{"0xd0", "0xf0"}) || !strings.Contains(c.Field, "master") {
		t.Fatalf("mixer master flag change %+v", c)
	}
	if ms, ok := mx.Parsed.(mixerStatus); !ok || ms.BPM != 128 {
		t.Fatalf("mixer parsed %+v", mx.Parsed)
	}
	air := find(r, cdj.PortBeat, 0x03, "DJM-V10")
	if air == nil || air.Length != 0x35 || air.Subtype != "0x03" {
		t.Fatalf("on-air stream %+v", air)
	}
	if c := changed(air, "0x25"); c == nil || c.Field != "ch2 on air" || c.Changes < 2 || changed(air, "0x24") != nil {
		t.Fatalf("on-air change map %+v", air.Changed)
	}
	if v, ok := air.Parsed.(map[string][]bool); !ok || len(v["on_air"]) != 6 || !v["on_air"][0] {
		t.Fatalf("on-air parsed %+v", air.Parsed)
	}
	if s := find(r, cdj.PortBeat, cdj.TypeBeat, "DJM-V10"); s == nil || s.Count < 2 {
		t.Fatalf("mixer beat stream %+v", s)
	}

	kas := find(r, cdj.PortDiscovery, cdj.TypeKeepalive, "CDJ-3000")
	if kas == nil || !slices.Contains(kas.Redacted, "0x26 mac") || !slices.Contains(kas.Redacted, "0x2c ip") {
		t.Fatalf("keepalive stream %+v", kas)
	}

	var usb, sd *mediaQuery
	for _, q := range r.MediaQueries {
		if q.Number == 2 && q.Slot == "usb" {
			usb = q
		} else if q.Number == 2 && q.Slot == "sd" {
			sd = q
		}
	}
	if usb == nil || !usb.Answered || sd == nil || sd.Answered || !strings.Contains(usb.RequestHex, "0a000001") {
		t.Fatalf("media queries usb %+v sd %+v", usb, sd)
	}
	mr := find(r, cdj.PortStatus, cdj.TypeMediaResponse, "CDJ-3000")
	if mr == nil {
		t.Fatal("no media response stream")
	}
	if m, ok := mr.Parsed.(*cdj.MediaResponse); !ok || m.MediaName != "PROBE USB" || m.TrackCount != 42 {
		t.Fatalf("media response parsed %+v", mr.Parsed)
	}

	// Player numbers above 4 are refused, so the probe (7) asks as player 2 itself.
	if len(setup) != 2 || setup[0] != 2 || setup[1] != 2 {
		t.Fatalf("dbserver setup asked as %v", setup)
	}
	var dbUSB, dbSD *dbReport
	for _, d := range r.DBServer {
		if d.Slot == "usb" {
			dbUSB = d
		} else {
			dbSD = d
		}
	}
	if dbUSB == nil || dbUSB.Error != "" || dbUSB.AskedAs != 2 || len(dbUSB.RootMenu) != 3 || dbUSB.RootMenu[2].Label != "TRACK" {
		t.Fatalf("dbserver usb %+v", dbUSB)
	}
	steps := map[string]int{}
	for _, s := range dbUSB.Transcript {
		steps[s.Dir+" "+s.Step+" "+s.Kind]++
	}
	if steps["tx setup 0x0000"] != 1 || steps["rx render item 0x4101"] != 3 || steps["rx render footer 0x4201"] != 1 {
		t.Fatalf("dbserver transcript %v", steps)
	}
	if dbSD == nil || !strings.Contains(dbSD.Error, "0x4003") {
		t.Fatalf("dbserver sd %+v", dbSD)
	}

	if len(r.Devices) != 3 { // CDJ-3000 #2, DJM-V10 #33, squatter #7; the probe itself is not a device
		t.Fatalf("devices %+v", r.Devices)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, mac := range []net.HardwareAddr{probeMAC, cdjMAC, djmMAC, squatterMAC} {
		if bytes.Contains(data, []byte(hex.EncodeToString(mac))) || bytes.Contains(data, []byte(mac.String())) {
			t.Fatalf("report leaks MAC %s", mac)
		}
	}
	if bytes.Contains(data, []byte("127.0.0.1")) {
		t.Fatal("report leaks an IP address")
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil || back["mode"] != "active" {
		t.Fatalf("report does not round-trip: %v", err)
	}
}

func TestPassiveNeverTransmits(t *testing.T) {
	b := newBench(t, true)
	b.p.cfg.Duration = 800 * time.Millisecond
	r := b.run()
	if b.isRegistered() || len(r.MediaQueries) != 0 || len(r.DBServer) != 0 || r.Probe != nil {
		t.Fatalf("passive probe transmitted: %+v", r)
	}
	if find(r, cdj.PortDiscovery, cdj.TypeKeepalive, "CDJ-3000") == nil || find(r, cdj.PortStatus, cdj.TypeStatus, "CDJ-3000") != nil {
		t.Fatalf("passive streams %+v", r.Streams)
	}
}
