package prolink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
)

// ExportConfig selects one mounted Rekordbox USB. Empty Root selects the first
// mounted export under /media. Address overrides allow isolated loopback tests.
type ExportConfig struct {
	Root                                                         string
	IP                                                           net.IP
	MAC                                                          net.HardwareAddr
	DBAddr, PortmapAddr, DiscoveryAddr, StatusAddr, AnnounceAddr string
	ProbeDuration                                                time.Duration
	// Observe receives valid status packets captured on the shared status port.
	Observe    func(Device)
	MediaCache *MediaCache // optional incoming status enrichment; allocated only while ON
}

type ExportStatus struct {
	Enabled          bool   `json:"enabled"`
	Number           byte   `json:"number"`
	Root             string `json:"root,omitempty"`
	DBAddress        string `json:"dbAddress,omitempty"`
	PortmapPort      int    `json:"portmapPort,omitempty"`
	DiscoveryAddress string `json:"discoveryAddress,omitempty"`
	StatusAddress    string `json:"statusAddress,omitempty"`
}

// Exporter is inert until SetEnabled(true): constructing it, asking its status,
// or disabling it binds no sockets and starts no goroutines or timers.
type Exporter struct {
	mu  sync.Mutex
	cfg ExportConfig
	run *exportRun
}

func NewExporter(cfg ExportConfig) *Exporter { return &Exporter{cfg: cfg} }
func (e *Exporter) SetEnabled(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !on {
		if e.run != nil {
			e.run.close()
			e.run = nil
		}
		return nil
	}
	if e.run != nil {
		return nil
	}
	r, err := startExport(e.cfg)
	if err != nil {
		return err
	}
	e.run = r
	return nil
}
func (e *Exporter) Close() { _ = e.SetEnabled(false) }
func (e *Exporter) Status() ExportStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return ExportStatus{}
	}
	r := e.run
	return ExportStatus{Enabled: true, Number: r.number(), Root: r.nfs.Root, DBAddress: r.db.Addr().String(), PortmapPort: r.nfs.PortmapPort(), DiscoveryAddress: r.discovery.LocalAddr().String(), StatusAddress: r.status.LocalAddr().String()}
}

type exportPeer struct {
	number byte
	seen   time.Time
	ip     net.IP
}
type exportRun struct {
	nfs               *NFSServer
	db                *DBServer
	discovery, status *net.UDPConn
	ip                net.IP
	mac               net.HardwareAddr
	announce          *net.UDPAddr
	media             *net.UDPAddr
	lib               *rekordbox.Library
	pdb               string
	pdbInfo           os.FileInfo
	capacity, free    uint64
	mu                sync.Mutex
	peers             map[string]exportPeer
	player            byte
	probed            bool
	mounted           bool
	first             bool
	statusCounter     uint32
	floodUntil        time.Time
	observe           func(Device)
	observer          *Discovery
	mediaCache        *MediaCache
	network           *net.IPNet
	done              chan struct{}
	wake              chan struct{}
	wg                sync.WaitGroup
}

// MountedExports does no polling and accepts read-only mounts too. Exporting
// never changes Engine's database or the Rekordbox drive.
func MountedExports() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var roots []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "/media/") {
			continue
		}
		root := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(fields[1])
		if st, err := os.Stat(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")); err == nil && st.Mode().IsRegular() {
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots
}

// Both the existing passive observer and the optional exporter need broadcast
// discovery. SO_REUSEADDR delivers broadcasts to both; no export socket exists
// until enabled. Unicast media requests go to the export listener.
func listenLinkUDP(address string, broadcast bool) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		if e := c.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			if err == nil && broadcast {
				err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}
		}); e != nil {
			return e
		}
		return err
	}}
	p, err := lc.ListenPacket(context.Background(), "udp4", address)
	if err != nil {
		return nil, err
	}
	return p.(*net.UDPConn), nil
}

func exportIdentity(cfg ExportConfig) (net.IP, net.HardwareAddr, net.IP, *net.IPNet, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			n, ok := address.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip := n.IP.To4()
			if cfg.IP != nil && !cfg.IP.IsUnspecified() {
				if !ip.Equal(cfg.IP) {
					// Explicit loopback identities can use isolated 127/8 test
					// addresses without adding system interface aliases.
					if len(cfg.MAC) == 6 && cfg.IP.IsLoopback() && ip.IsLoopback() && n.Contains(cfg.IP) {
						ip = cfg.IP.To4()
					} else {
						continue
					}
				}
			} else if ip.IsLoopback() || len(iface.HardwareAddr) != 6 {
				continue
			}
			mac := iface.HardwareAddr
			if len(cfg.MAC) != 0 {
				mac = cfg.MAC
			}
			if len(mac) != 6 {
				return nil, nil, nil, nil, errors.New("prolink: export needs a six-byte interface MAC")
			}
			broadcast := append(net.IP(nil), ip...)
			mask := n.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			for i := range broadcast {
				broadcast[i] |= ^mask[i]
			}
			return append(net.IP(nil), ip...), append(net.HardwareAddr(nil), mac...), broadcast, &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
		}
	}
	return nil, nil, nil, nil, errors.New("prolink: no IPv4 interface for export")
}

func startExport(cfg ExportConfig) (*exportRun, error) {
	root := cfg.Root
	if root == "" {
		roots := MountedExports()
		if len(roots) == 0 {
			return nil, errors.New("prolink: mount a Rekordbox USB export first")
		}
		root = roots[0]
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("prolink: export root must be a real directory")
	}
	nfs := &NFSServer{Root: root}
	rel := ""
	for _, part := range []string{"PIONEER", "rekordbox", "export.pdb"} {
		var status uint32
		rel, status = nfs.child(rel, part)
		if status != nfsOK {
			return nil, errors.New("prolink: invalid export.pdb path")
		}
		if _, status = nfs.stat(rel); status != nfsOK {
			return nil, errors.New("prolink: export.pdb must not follow symlinks")
		}
	}
	pdb := filepath.Join(root, rel)
	fs, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer fs.Close()
	f, err := fs.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("prolink: export.pdb must be a regular file")
	}
	lib, err := rekordbox.ParsePDB(f, st.Size())
	if err != nil {
		return nil, err
	}
	if err := lib.Validate(); err != nil {
		return nil, err
	}
	for i := range lib.Tracks {
		t := &lib.Tracks[i]
		for _, p := range []*string{&t.Path, &t.AnalysisPath} {
			if *p == "" {
				continue
			}
			rel, ok := SafeRel(*p)
			if !ok {
				return nil, fmt.Errorf("prolink: unsafe path in track %d", t.ID)
			}
			*p = "/" + filepath.ToSlash(rel)
		}
	}
	ip, mac, broadcast, network, err := exportIdentity(cfg)
	if err != nil {
		return nil, err
	}
	address := func(given string, port string) string {
		if given != "" {
			return given
		}
		return net.JoinHostPort(ip.String(), port)
	}
	r := &exportRun{network: network, observe: cfg.Observe, nfs: nfs, ip: ip, mac: mac, lib: lib, pdb: pdb, pdbInfo: st, peers: make(map[string]exportPeer), mounted: true, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	if cfg.MediaCache != nil {
		r.observer = NewDiscovery()
		r.observer.SetMediaCache(cfg.MediaCache)
		r.mediaCache = cfg.MediaCache
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) == nil {
		r.capacity = uint64(disk.Blocks) * uint64(disk.Bsize)
		r.free = uint64(disk.Bavail) * uint64(disk.Bsize)
	}
	nfs.Available = r.mediaAvailable
	discoveryAddress := cfg.DiscoveryAddr
	if discoveryAddress == "" {
		discoveryAddress = "0.0.0.0:50000"
	}
	r.discovery, err = listenLinkUDP(discoveryAddress, true)
	if err != nil {
		return nil, err
	}
	r.status, err = listenLinkUDP(address(cfg.StatusAddr, "50002"), true)
	if err != nil {
		r.discovery.Close()
		return nil, err
	}
	announce := cfg.AnnounceAddr
	if announce == "" {
		announce = net.JoinHostPort(broadcast.String(), "50000")
	}
	r.announce, err = net.ResolveUDPAddr("udp4", announce)
	if err != nil {
		r.status.Close()
		r.discovery.Close()
		return nil, err
	}
	r.media = &net.UDPAddr{IP: r.announce.IP, Port: 50002}
	if err = nfs.Listen(address(cfg.PortmapAddr, "111")); err != nil {
		r.status.Close()
		r.discovery.Close()
		return nil, err
	}
	r.db = &DBServer{Library: lib, Number: r.number}
	if err = r.db.Listen(address(cfg.DBAddr, "12523")); err != nil {
		nfs.Close()
		r.status.Close()
		r.discovery.Close()
		return nil, err
	}
	probe := cfg.ProbeDuration
	if probe <= 0 {
		probe = 2500 * time.Millisecond // captured keepalive cadence can exceed two seconds
	}
	r.wg.Add(3)
	go r.receivePeers()
	go r.receiveMedia()
	go r.announceLoop(probe)
	return r, nil
}
func (r *exportRun) number() byte         { r.mu.Lock(); defer r.mu.Unlock(); return r.player }
func (r *exportRun) mediaAvailable() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.mounted }
func (r *exportRun) close() {
	close(r.done)
	r.discovery.Close()
	r.status.Close()
	r.mu.Lock()
	r.player = 0
	r.mu.Unlock()
	r.db.Close()
	r.nfs.Close()
	r.wg.Wait()
}
func (r *exportRun) chooseLocked(now time.Time) {
	used := [5]bool{}
	for key, p := range r.peers {
		if now.Sub(p.seen) > 10*time.Second {
			delete(r.peers, key)
		} else {
			if p.number <= 4 {
				used[p.number] = true
			}
		}
	}
	old := r.player
	r.player = 0
	if !r.probed || !r.mounted || now.Before(r.floodUntil) {
		return
	}
	if old > 0 && !used[old] {
		r.player = old
		return
	}
	for n := byte(4); n > 0; n-- {
		if !used[n] {
			r.player = n
			return
		}
	}
}
func (r *exportRun) receivePeers() {
	defer r.wg.Done()
	b := make([]byte, 2048)
	for {
		n, addr, err := r.discovery.ReadFromUDP(b)
		if err != nil {
			return
		}
		d, err := ParsePacket(b[:n], 50000, addr.IP)
		if err != nil || d.Number < 1 || d.Number > 6 || !r.network.Contains(addr.IP) {
			continue
		}
		mac := b[0x26:0x2c]
		if addr.IP.Equal(r.ip) && bytes.Equal(mac, r.mac) && d.Name == "EngineX Export Beta" {
			continue
		}
		key := addr.IP.String() + "/" + net.HardwareAddr(mac).String()
		r.mu.Lock()
		if _, ok := r.peers[key]; !ok && len(r.peers) >= 64 {
			r.player = 0
			r.floodUntil = time.Now().Add(10 * time.Second) // resume after the bounded peer window expires
			r.mu.Unlock()
			r.db.closeClients()
			continue
		}
		r.peers[key] = exportPeer{number: d.Number, seen: time.Now(), ip: append(net.IP(nil), addr.IP...)}
		old := r.player
		// No disk, menu work, or periodic timer on the collision path.
		if old == d.Number {
			r.chooseLocked(time.Now())
		}
		changed := old != r.player
		r.mu.Unlock()
		if changed {
			r.db.closeClients()
			select {
			case r.wake <- struct{}{}:
			default:
			}
		}
	}
}
func (r *exportRun) receiveMedia() {
	defer r.wg.Done()
	b := make([]byte, 2048)
	for {
		n, addr, err := r.status.ReadFromUDP(b)
		if err != nil {
			return
		}
		if r.observe != nil {
			if d, err := ParsePacket(b[:n], 50002, addr.IP); err == nil && d.Number >= 1 && d.Number <= 6 && r.network.Contains(addr.IP) {
				if r.observer != nil && (r.mediaCache.Enabled == nil || r.mediaCache.Enabled()) {
					if d.SourcePlayer != d.Number {
						r.mu.Lock()
						var source net.IP
						for _, p := range r.peers {
							if p.number == d.SourcePlayer {
								source = p.ip
								break
							}
						}
						r.mu.Unlock()
						if source != nil {
							r.observer.Observe(Device{Number: d.SourcePlayer, Address: source.String(), PacketType: 6})
						}
					}
					d = r.observer.Observe(d)
				}
				r.observe(d)
			}
		}
		if n < 0x30 || string(b[:10]) != magic || b[10] != 5 {
			continue
		}
		r.mu.Lock()
		if r.player != 0 && b[0x2b] == r.player && b[0x2f] == 3 {
			r.sendLocked(r.status, r.mediaPacket(), addr)
		}
		r.mu.Unlock()
	}
}
func (r *exportRun) sendLocked(c *net.UDPConn, b []byte, to *net.UDPAddr) {
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	_, _ = c.WriteToUDP(b, to)
}
func (r *exportRun) announceLoop(probe time.Duration) {
	defer r.wg.Done()
	timer := time.NewTimer(probe)
	defer timer.Stop()
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	statusTicker := time.NewTicker(200 * time.Millisecond)
	defer statusTicker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-timer.C:
			r.mu.Lock()
			r.probed = true
			r.first = len(r.peers) == 0
			r.chooseLocked(time.Now())
			r.mu.Unlock()
		case <-statusTicker.C:
			r.sendStatus()
			continue
		case <-ticker.C:
			st, err := os.Stat(r.pdb)
			r.mu.Lock()
			old := r.player
			r.mounted = err == nil && os.SameFile(r.pdbInfo, st) && r.pdbInfo.ModTime().Equal(st.ModTime()) && r.pdbInfo.Size() == st.Size()
			r.chooseLocked(time.Now())
			changed := old != r.player
			r.mu.Unlock()
			if changed {
				r.db.closeClients()
			}
		case <-r.wake:
		}
		// Each send holds the identity lock for at most its 10ms deadline, so a
		// received claim cannot be followed by a queued old-number announcement.
		r.mu.Lock()
		if r.player != 0 {
			r.sendLocked(r.discovery, r.announcement(), r.announce)
		}
		r.mu.Unlock()
		r.mu.Lock()
		if r.player != 0 {
			r.sendLocked(r.status, r.mediaPacket(), r.media)
		}
		r.mu.Unlock()
	}
}
func linkPacket(kind byte, length, nameOffset int) []byte {
	b := make([]byte, length)
	copy(b, magic)
	b[10] = kind
	copy(b[nameOffset:nameOffset+20], "EngineX Export Beta")
	b[nameOffset+20] = 1
	return b
}
func (r *exportRun) announcement() []byte {
	b := linkPacket(6, 0x36, 12)
	b[0x21] = 2
	binary.BigEndian.PutUint16(b[0x22:], 0x36)
	b[0x24], b[0x25] = r.player, 1
	if r.first {
		b[0x25] = 2
	}
	copy(b[0x26:], r.mac)
	copy(b[0x2c:], r.ip.To4())
	b[0x30], b[0x34] = byte(min(len(r.peers)+1, 6)), 1
	return b
}
func (r *exportRun) mediaPacket() []byte {
	b := linkPacket(6, 0xc0, 11)
	b[0x21] = r.player
	binary.BigEndian.PutUint16(b[0x22:], 0x9c)
	b[0x27], b[0x2b] = r.player, 3
	u := utf16.Encode([]rune(filepath.Base(r.nfs.Root)))
	limit := min(len(u), 31)
	if limit > 0 && u[limit-1] >= 0xd800 && u[limit-1] <= 0xdbff {
		limit--
	}
	for i, n := range u[:limit] {
		binary.BigEndian.PutUint16(b[0x2c+2*i:], n)
	}
	binary.BigEndian.PutUint16(b[0xa6:], uint16(min(len(r.lib.Tracks), 65535)))
	b[0xaa] = 1
	binary.BigEndian.PutUint16(b[0xae:], uint16(min(len(r.lib.Playlists), 65535)))
	binary.BigEndian.PutUint64(b[0xb0:], r.capacity)
	binary.BigEndian.PutUint64(b[0xb8:], r.free)
	return b
}

// Real captures and Beat Link use 0x11c bytes for the extended legacy status
// layout (not 0x110, which is the offset of its final row). Advertise this
// common layout; native CDJ-3000 status extensions are not implemented.
func (r *exportRun) statusPacket() []byte {
	b := linkPacket(0x0a, 0x11c, 11)
	b[0x20], b[0x21], b[0x24], b[0x26] = 4, r.player, r.player, 1
	binary.BigEndian.PutUint16(b[0x22:], uint16(len(b)-0x24))
	b[0x6a], b[0x6b], b[0x6f], b[0x73] = 4, 4, 0, 4 // USB mounted, no SD
	b[0x75] = 1                                     // LINK available while the exported USB is mounted
	binary.BigEndian.PutUint16(b[0x90:], 0x7fff)    // no loaded track / meaningful master BPM
	copy(b[0x7c:0x80], "1.00")
	b[0x89], b[0x8b], b[0x9f], b[0xcc] = 0x84, 0x7e, 0xff, 0x0f // idle, no master/sync
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		binary.BigEndian.PutUint32(b[off:], 0x00100000)
	}
	binary.BigEndian.PutUint16(b[0x92:], 0xffff)
	binary.BigEndian.PutUint32(b[0xa0:], 0xffffffff)
	r.statusCounter++
	binary.BigEndian.PutUint32(b[0xc8:], r.statusCounter)
	return b
}
func (r *exportRun) sendStatus() {
	r.mu.Lock()
	peers := make([]net.IP, 0, len(r.peers))
	seen := make(map[string]bool)
	for _, p := range r.peers {
		if time.Since(p.seen) <= 10*time.Second && !seen[p.ip.String()] && len(peers) < 6 {
			peers = append(peers, p.ip)
			seen[p.ip.String()] = true
		}
	}
	r.mu.Unlock()
	for _, ip := range peers {
		r.mu.Lock()
		if r.player != 0 {
			r.sendLocked(r.status, r.statusPacket(), &net.UDPAddr{IP: ip, Port: 50002})
		}
		r.mu.Unlock()
	}
}
