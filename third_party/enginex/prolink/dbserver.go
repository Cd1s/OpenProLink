package prolink

// Read-only dbserver subset for browsing one USB export. Wire references:
// https://djl-analysis.deepsymmetry.org/djl-analysis/track_metadata.html
// https://djl-analysis.deepsymmetry.org/djl-analysis/menus.html

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/rekordbox"
)

const dbMagic uint32 = 0x872349ae

type dbField struct {
	tag  byte
	n    uint32
	data []byte
}

func dbNumber(n uint32) dbField { return dbField{tag: 0x11, n: n} }
func dbString(s string) dbField {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*(len(u)+1))
	for i, n := range u {
		binary.BigEndian.PutUint16(b[2*i:], n)
	}
	return dbField{tag: 0x26, data: b}
}
func (f dbField) text() string {
	u := make([]uint16, len(f.data)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(f.data[2*i:])
	}
	return strings.TrimSuffix(string(utf16.Decode(u)), "\x00")
}
func (f dbField) argumentTag() byte {
	switch f.tag {
	case 0x11:
		return 6
	case 0x26:
		return 2
	case 0x14:
		return 3
	}
	return 0
}
func (f dbField) appendTo(b []byte) []byte {
	b = append(b, f.tag)
	switch f.tag {
	case 0x0f:
		return append(b, byte(f.n))
	case 0x10:
		return binary.BigEndian.AppendUint16(b, uint16(f.n))
	case 0x11:
		return binary.BigEndian.AppendUint32(b, f.n)
	default:
		n := len(f.data)
		if f.tag == 0x26 {
			n /= 2
		}
		b = binary.BigEndian.AppendUint32(b, uint32(n))
		return append(b, f.data...)
	}
}
func readDBField(r io.Reader) (dbField, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:1]); err != nil {
		return dbField{}, err
	}
	f := dbField{tag: head[0]}
	n := 4
	switch f.tag {
	case 0x0f:
		n = 1
	case 0x10:
		n = 2
	case 0x11, 0x14, 0x26:
	default:
		return f, errors.New("prolink: invalid dbserver field")
	}
	if _, err := io.ReadFull(r, head[1:n+1]); err != nil {
		return f, err
	}
	for _, b := range head[1 : n+1] {
		f.n = f.n<<8 | uint32(b)
	}
	if f.tag == 0x14 || f.tag == 0x26 {
		// Bound untrusted lengths before multiplying or allocating.
		if f.n > 65536 {
			return f, errors.New("prolink: dbserver field too large")
		}
		n = int(f.n)
		if f.tag == 0x26 {
			n *= 2
		}
		f.data = make([]byte, n)
		if _, err := io.ReadFull(r, f.data); err != nil {
			return f, err
		}
		if f.tag == 0x26 && (n < 2 || f.data[n-1] != 0 || f.data[n-2] != 0) {
			return f, errors.New("prolink: unterminated dbserver string")
		}
	}
	return f, nil
}

type dbMessage struct {
	tx   uint32
	kind uint16
	args []dbField
}

func (m dbMessage) bytes() []byte {
	b := dbNumber(dbMagic).appendTo(nil)
	b = dbNumber(m.tx).appendTo(b)
	b = (dbField{tag: 0x10, n: uint32(m.kind)}).appendTo(b)
	b = (dbField{tag: 0x0f, n: uint32(len(m.args))}).appendTo(b)
	tags := make([]byte, 12)
	for i, f := range m.args {
		tags[i] = f.argumentTag()
	}
	b = (dbField{tag: 0x14, data: tags}).appendTo(b)
	for _, f := range m.args {
		b = f.appendTo(b)
	}
	return b
}
func readDBMessage(r io.Reader) (dbMessage, error) {
	var h [5]dbField
	for i := range h {
		f, err := readDBField(r)
		if err != nil {
			return dbMessage{}, err
		}
		h[i] = f
	}
	if h[0].tag != 0x11 || h[0].n != dbMagic || h[1].tag != 0x11 || h[2].tag != 0x10 || h[3].tag != 0x0f || h[3].n > 12 || h[4].tag != 0x14 || len(h[4].data) != 12 {
		return dbMessage{}, errors.New("prolink: invalid dbserver header")
	}
	m := dbMessage{tx: h[1].n, kind: uint16(h[2].n)}
	for i := uint32(0); i < h[3].n; i++ {
		f, err := readDBField(r)
		if err != nil {
			return m, err
		}
		if f.argumentTag() == 0 || f.argumentTag() != h[4].data[i] {
			return m, errors.New("prolink: mismatched dbserver argument tag")
		}
		m.args = append(m.args, f)
	}
	return m, nil
}

type dbItem struct {
	parent, id, kind, position uint32
	label, secondary           string
}

func (i dbItem) fields() []dbField {
	a, b := dbString(i.label), dbString(i.secondary)
	return []dbField{dbNumber(i.parent), dbNumber(i.id), dbNumber(uint32(len(a.data))), a, dbNumber(uint32(len(b.data))), b, dbNumber(i.kind), dbNumber(0), dbNumber(0), dbNumber(i.position), dbNumber(0), dbNumber(0)}
}

// DBServer implements the TCP 12523 port query and dbserver on the same port.
// dbserver is not an ONC RPC program: the port query, rather than portmapper,
// tells a CDJ where to connect. Library is immutable while listening.
type DBServer struct {
	Library    *rekordbox.Library
	Number     func() byte // zero means probing, collision, or unavailable media
	mu         sync.Mutex
	listener   net.Listener
	clients    map[net.Conn]bool
	tracks     map[uint32]rekordbox.Track
	trackItems []dbItem
	wg         sync.WaitGroup
}

func (s *DBServer) Listen(address string) error {
	if s.Library == nil {
		return errors.New("prolink: dbserver needs a Rekordbox library")
	}
	s.tracks = make(map[uint32]rekordbox.Track, len(s.Library.Tracks))
	for _, t := range s.Library.Tracks {
		s.tracks[t.ID] = t
	}
	s.trackItems = make([]dbItem, 0, len(s.Library.Tracks))
	for _, t := range s.Library.Tracks {
		s.trackItems = append(s.trackItems, dbItem{id: t.ID, kind: 0x0704, label: t.Title, secondary: t.Artist})
	}
	sort.SliceStable(s.trackItems, func(i, j int) bool {
		return strings.ToUpper(s.trackItems[i].label) < strings.ToUpper(s.trackItems[j].label)
	})
	l, err := net.Listen("tcp4", address)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener, s.clients = l, make(map[net.Conn]bool)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.listener == nil || len(s.clients) >= 16 {
				s.mu.Unlock()
				c.Close()
				continue
			}
			s.clients[c] = true
			s.wg.Add(1)
			s.mu.Unlock()
			go func() {
				defer s.wg.Done()
				defer func() { c.Close(); s.mu.Lock(); delete(s.clients, c); s.mu.Unlock() }()
				s.serve(c, uint16(l.Addr().(*net.TCPAddr).Port))
			}()
		}
	}()
	return nil
}
func (s *DBServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}
func (s *DBServer) closeClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.Close()
	}
}
func (s *DBServer) Close() {
	s.mu.Lock()
	l := s.listener
	s.listener = nil
	if l != nil {
		l.Close()
	}
	for c := range s.clients {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *DBServer) number() byte {
	if s.Number == nil {
		return 0
	}
	return s.Number()
}
func (s *DBServer) serve(c net.Conn, port uint16) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	first, err := r.Peek(5)
	if err != nil {
		return
	}
	if bytes.Equal(first, []byte{0, 0, 0, 15, 'R'}) {
		q := make([]byte, 19)
		if _, err := io.ReadFull(r, q); err != nil || string(q[4:]) != "RemoteDBServer\x00" || s.number() == 0 {
			return
		}
		_, _ = c.Write(binary.BigEndian.AppendUint16(nil, port))
		return
	}
	f, err := readDBField(r)
	if err != nil || f.tag != 0x11 || f.n != 1 {
		return
	}
	if _, err := c.Write(f.appendTo(nil)); err != nil {
		return
	}
	menus := make(map[uint32][]dbItem)
	var menuOrder []uint32
	introduced := false
	var requester byte
	for {
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		m, err := readDBMessage(r)
		if err != nil {
			return
		}
		number := s.number()
		if number == 0 {
			return
		}
		if m.kind == 0x0100 {
			return
		}
		reply := func(kind uint16, args ...dbField) error {
			_, err := c.Write((dbMessage{m.tx, kind, args}).bytes())
			return err
		}
		unavailable := func() error { return reply(0x4003, dbNumber(uint32(m.kind))) }
		if m.kind == 0 {
			if len(m.args) != 1 || m.args[0].tag != 0x11 || m.args[0].n < 1 || m.args[0].n > 6 {
				return
			}
			introduced = true
			requester = byte(m.args[0].n)
			if reply(0x4000, dbNumber(0), dbNumber(uint32(number))) != nil {
				return
			}
			continue
		}
		if !introduced {
			return
		}
		valid := len(m.args) > 0
		for _, f := range m.args {
			valid = valid && f.tag == 0x11
		}
		if !valid || byte(m.args[0].n>>16) > 0x0f || byte(m.args[0].n>>24) != requester || byte(m.args[0].n>>8) != 3 || byte(m.args[0].n) != 1 {
			if unavailable() != nil {
				return
			}
			continue
		}
		if m.kind == 0x3000 {
			items := menus[m.args[0].n]
			if len(m.args) != 6 || items == nil {
				if unavailable() != nil {
					return
				}
				continue
			}
			if reply(0x4001, dbNumber(1), dbNumber(0)) != nil {
				return
			}
			start := min(uint64(m.args[1].n), uint64(len(items)))
			end := min(start+uint64(m.args[2].n), uint64(len(items)))
			for _, item := range items[start:end] {
				if reply(0x4101, item.fields()...) != nil {
					return
				}
			}
			if reply(0x4201) != nil {
				return
			}
			continue
		}
		items, err := s.query(m)
		if err != nil {
			if unavailable() != nil {
				return
			}
			continue
		}
		// Menu locations are independent (main, submenu, metadata, etc.).
		key := m.args[0].n
		if _, ok := menus[key]; !ok {
			if len(menuOrder) == 4 {
				delete(menus, menuOrder[0])
				menuOrder = menuOrder[1:]
			}
			menuOrder = append(menuOrder, key)
		}
		menus[key] = items
		if reply(0x4000, dbNumber(uint32(m.kind)), dbNumber(uint32(len(items)))) != nil {
			return
		}
	}
}
func (s *DBServer) query(m dbMessage) ([]dbItem, error) {
	items := make([]dbItem, 0)
	track := func(t rekordbox.Track, position uint32) dbItem {
		return dbItem{id: t.ID, kind: 0x0704, label: t.Title, secondary: t.Artist, position: position}
	}
	switch m.kind {
	case 0x1000:
		if len(m.args) != 3 {
			break
		}
		return []dbItem{{id: 5, kind: 0x84, label: "\ufffaPLAYLIST\ufffb"}, {id: 4, kind: 0x83, label: "\ufffaTRACK\ufffb"}}, nil
	case 0x1004:
		if len(m.args) != 2 {
			break
		}
		return s.trackItems, nil
	case 0x1105:
		if len(m.args) != 4 {
			break
		}
		id, folder := m.args[2].n, m.args[3].n
		if folder == 1 {
			if id != 0 {
				found := false
				for _, p := range s.Library.Playlists {
					found = found || p.ID == id && p.Folder
				}
				if !found {
					break
				}
			}
			for _, p := range s.Library.Playlists {
				if p.ParentID == id {
					kind := uint32(8)
					if p.Folder {
						kind = 1
					}
					items = append(items, dbItem{parent: id, id: p.ID, kind: kind, label: p.Name})
				}
			}
			return items, nil
		}
		if folder == 0 {
			for _, p := range s.Library.Playlists {
				if p.ID != id || p.Folder {
					continue
				}
				for i, tid := range p.Tracks {
					if t, ok := s.tracks[tid]; ok {
						items = append(items, track(t, uint32(i+1)))
					}
				}
				return items, nil
			}
		}
	case 0x2002, 0x2102:
		if len(m.args) != 2 {
			break
		}
		t, ok := s.tracks[m.args[1].n]
		if !ok {
			break
		}
		{
			if m.kind == 0x2102 {
				// Get Track Information returns one audio path item. ANLZ paths
				// come from export.pdb over NFS, not an invented second item.
				items = append(items, dbItem{parent: uint32(t.FileSize), id: t.ID, kind: 0, label: t.Path})
				return items, nil
			}
			return []dbItem{{id: t.ID, kind: 4, label: t.Title}, {kind: 7, label: t.Artist}, {kind: 2, label: t.Album}, {kind: 0xb, id: uint32(t.Duration)}, {kind: 0xd, id: uint32(t.BPM * 100)}, {kind: 0xf, label: t.Key}, {kind: 0x23, label: t.Comment}}, nil
		}
	}
	return nil, fmt.Errorf("prolink: unavailable dbserver query 0x%04x", m.kind)
}
