package prolink

// Read-only portmapper + MOUNT v1 + NFS v2 over UDP, serving one drive as a
// player's USB slot ("/C/") the way Pioneer players export theirs (#63). It is
// the server side of nfs.go. Requests come from anyone on the LAN, so:
// nothing is ever written, every name is one plain path component, symlinks
// are refused at every step, and file handles are opaque indexes into a
// bounded table, so a handle can never name a path outside the root.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

const (
	nfsOK     = 0
	nfsNoEnt  = 2
	nfsIO     = 5
	nfsAccess = 13
	nfsNotDir = 20
	nfsROFS   = 30
	nfsStale  = 70
	maxHandle = 1 << 17 // paths remembered before the table restarts (old handles go stale)
)

// NFSServer serves Root. Start it with Serve; Close stops it.
type NFSServer struct {
	Root      string
	Available func() bool // optional media-generation guard

	mu    sync.Mutex
	gen   uint32
	paths []string // handle index -> path relative to Root ("" is the root)
	index map[string]uint32
	conns []*net.UDPConn
	fs    *os.Root // pins the mounted directory; path replacement cannot escape it
	wg    sync.WaitGroup
}

// Listen binds the portmapper on pmAddr (":111" on a deck) and MOUNT+NFS on
// one ephemeral port of the same IP.
func (s *NFSServer) Listen(pmAddr string) error {
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			root.Close()
		}
	}()
	pa, err := net.ResolveUDPAddr("udp4", pmAddr)
	if err != nil {
		return err
	}
	pm, err := net.ListenUDP("udp4", pa)
	if err != nil {
		return err
	}
	svc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: pa.IP})
	if err != nil {
		pm.Close()
		return err
	}
	var g [4]byte
	_, _ = rand.Read(g[:])
	s.mu.Lock()
	s.gen = binary.BigEndian.Uint32(g[:])
	s.paths, s.index = []string{""}, map[string]uint32{"": 0}
	s.conns = []*net.UDPConn{pm, svc}
	s.fs = root
	s.mu.Unlock()
	port := uint32(svc.LocalAddr().(*net.UDPAddr).Port)
	for _, c := range s.conns {
		s.wg.Add(1)
		go func(c *net.UDPConn) {
			defer s.wg.Done()
			s.serve(c, port)
		}(c)
	}
	started = true
	return nil
}

// PortmapPort is the bound portmapper port (for tests on an ephemeral port).
func (s *NFSServer) PortmapPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return 0
	}
	return s.conns[0].LocalAddr().(*net.UDPAddr).Port
}

// Close closes both sockets and waits for the goroutines to end.
func (s *NFSServer) Close() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
	s.mu.Lock()
	if s.fs != nil {
		s.fs.Close()
		s.fs = nil
	}
	s.mu.Unlock()
}

func (s *NFSServer) serve(c *net.UDPConn, port uint32) {
	buf := make([]byte, 9000)
	for {
		n, addr, err := c.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if reply := s.handle(buf[:n], port); reply != nil {
			_, _ = c.WriteToUDP(reply, addr)
		}
	}
}

func decodeUTF16LE(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// handle answers one RPC call, or returns nil to drop it.
func (s *NFSServer) handle(b []byte, port uint32) []byte {
	r := &reader{b: b}
	xid := r.u32()
	if r.u32() != 0 || r.u32() != 2 { // CALL, RPC v2
		return nil
	}
	prog, vers, proc := r.u32(), r.u32(), r.u32()
	r.u32()
	r.opaque(400) // credentials
	r.u32()
	r.opaque(400) // verifier
	if r.err != nil {
		return nil
	}
	out := &xdr{}
	out.u32(xid).u32(1).u32(0).u32(0).u32(0) // REPLY, MSG_ACCEPTED, AUTH_NULL verifier
	accept := func(st uint32) []byte { return out.u32(st).Bytes() }
	switch {
	case prog == rpcPortmap && vers == 2 && proc == 0, prog == rpcMount && proc == 0, prog == rpcNFS && vers == 2 && proc == 0:
		return accept(0) // NULL
	case prog == rpcPortmap && vers == 2 && proc == 3: // GETPORT
		p, v := r.u32(), r.u32()
		protocol := r.u32()
		r.u32()
		if r.err != nil {
			return nil
		}
		out.u32(0)
		if protocol == 17 && (p == rpcMount && v == 1 || p == rpcNFS && v == 2) {
			return out.u32(port).Bytes()
		}
		return out.u32(0).Bytes()
	case prog == rpcMount && vers == 1 && proc == 1: // MNT
		dir := r.opaque(1024)
		if r.err != nil {
			return nil
		}
		out.u32(0)
		if p := decodeUTF16LE(dir); p != "/C/" && p != "/C" {
			return out.u32(nfsNoEnt).Bytes()
		}
		out.u32(nfsOK)
		out.Write(s.fh(""))
		return out.Bytes()
	case prog == rpcMount && vers == 1 && proc == 3: // UMNT
		return accept(0)
	case prog == rpcNFS && vers == 2:
		out.u32(0)
		if s.Available != nil && !s.Available() {
			return out.u32(nfsStale).Bytes()
		}
		return s.nfs(proc, r, out)
	case prog == rpcPortmap || prog == rpcMount || prog == rpcNFS:
		return accept(3) // PROC_UNAVAIL
	}
	return accept(1) // PROG_UNAVAIL
}

func (s *NFSServer) nfs(proc uint32, r *reader, out *xdr) []byte {
	switch proc {
	case 2, 8, 9, 10, 11, 12, 13, 14, 15: // SETATTR, WRITE, CREATE, REMOVE, RENAME, LINK, SYMLINK, MKDIR, RMDIR
		return out.u32(nfsROFS).Bytes()
	}
	rel, ok := s.lookupFH(r.fixed(fhSize))
	if r.err != nil {
		return nil
	}
	if !ok {
		return out.u32(nfsStale).Bytes()
	}
	switch proc {
	case 1: // GETATTR
		fi, st := s.stat(rel)
		if st != nfsOK {
			return out.u32(st).Bytes()
		}
		out.u32(nfsOK)
		s.fattr(out, rel, fi)
	case 4: // LOOKUP
		name := decodeUTF16LE(r.opaque(1024))
		if r.err != nil {
			return nil
		}
		child, st := s.child(rel, name)
		if st != nfsOK {
			return out.u32(st).Bytes()
		}
		fi, st := s.stat(child)
		if st != nfsOK {
			return out.u32(st).Bytes()
		}
		out.u32(nfsOK)
		out.Write(s.fh(child))
		s.fattr(out, child, fi)
	case 6: // READ
		off, count := r.u32(), r.u32()
		r.u32()
		if r.err != nil {
			return nil
		}
		fi, st := s.stat(rel)
		if st == nfsOK && !fi.Mode().IsRegular() {
			st = nfsAccess
		}
		var data []byte
		if st == nfsOK {
			data, st = s.read(rel, off, min(count, nfsChunk))
		}
		if st != nfsOK {
			return out.u32(st).Bytes()
		}
		out.u32(nfsOK)
		s.fattr(out, rel, fi)
		out.opaque(data)
	case 16: // READDIR
		cookie, count := r.u32(), r.u32()
		if r.err != nil {
			return nil
		}
		fi, st := s.stat(rel)
		if st == nfsOK && !fi.IsDir() {
			st = nfsNotDir
		}
		if st != nfsOK {
			return out.u32(st).Bytes()
		}
		entries, err := s.readDir(rel)
		if err != nil {
			return out.u32(nfsIO).Bytes()
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		out.u32(nfsOK)
		budget, eof := int(min(count, 8192))-16, uint32(1)
		for i := int(min(uint64(cookie), uint64(len(entries)))); i < len(entries); i++ {
			e := entries[i]
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			name := pioneerName(e.Name())
			if budget -= 16 + len(name) + 3; budget < 0 {
				eof = 0
				break
			}
			out.u32(1).u32(fileID(path.Join(rel, e.Name()))).opaque(name).u32(uint32(i + 1))
		}
		out.u32(0).u32(eof)
	case 17: // STATFS
		var fs syscall.Statfs_t
		if err := syscall.Statfs(s.Root, &fs); err != nil {
			return out.u32(nfsIO).Bytes()
		}
		out.u32(nfsOK).u32(nfsChunk).u32(uint32(fs.Bsize)).u32(uint32(min(uint64(fs.Blocks), uint64(1<<32-1)))).u32(uint32(min(uint64(fs.Bfree), uint64(1<<32-1)))).u32(uint32(min(uint64(fs.Bavail), uint64(1<<32-1))))
	default:
		out.Truncate(out.Len() - 4)
		return out.u32(3).Bytes() // PROC_UNAVAIL
	}
	return out.Bytes()
}

// child resolves one path component below dir. Names that are not a single
// plain component are refused; a case-insensitive match is accepted, as on
// the FAT/exFAT sticks players use.
func (s *NFSServer) child(dir, name string) (string, uint32) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", nfsNoEnt
	}
	rel := path.Join(dir, name)
	if _, err := s.lstat(rel); err == nil {
		return rel, nfsOK
	}
	entries, err := s.readDir(dir)
	if err != nil {
		return "", nfsNoEnt
	}
	for _, e := range entries {
		if strings.EqualFold(norm.NFC.String(e.Name()), norm.NFC.String(name)) {
			return path.Join(dir, e.Name()), nfsOK
		}
	}
	return "", nfsNoEnt
}

// stat refuses a path if any component is a symlink.
func (s *NFSServer) stat(rel string) (os.FileInfo, uint32) {
	p := ""
	var fi os.FileInfo
	var err error
	parts := strings.Split(rel, "/")
	if rel == "" {
		parts = nil
	}
	if fi, err = s.lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return nil, nfsNoEnt
	}
	for _, part := range parts {
		p = filepath.Join(p, part)
		if fi, err = s.lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return nil, nfsNoEnt
		}
	}
	return fi, nfsOK
}

func (s *NFSServer) lstat(rel string) (os.FileInfo, error) {
	if s.fs != nil {
		if rel == "" {
			rel = "."
		}
		return s.fs.Lstat(rel)
	}
	return os.Lstat(filepath.Join(s.Root, rel))
}
func (s *NFSServer) readDir(rel string) ([]os.DirEntry, error) {
	if s.fs == nil {
		return os.ReadDir(filepath.Join(s.Root, rel))
	}
	if rel == "" {
		rel = "."
	}
	f, err := s.fs.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func (s *NFSServer) read(rel string, off, count uint32) ([]byte, uint32) {
	var f *os.File
	var err error
	if s.fs != nil {
		f, err = s.fs.Open(rel)
	} else {
		f, err = os.Open(filepath.Join(s.Root, rel))
	}
	if err != nil {
		return nil, nfsIO
	}
	defer f.Close()
	b := make([]byte, count)
	n, err := f.ReadAt(b, int64(off))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nfsIO
	}
	return b[:n], nfsOK
}

func fileID(rel string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(rel))
	return h.Sum32() | 1
}

func (s *NFSServer) fattr(out *xdr, rel string, fi os.FileInfo) {
	kind, mode := uint32(1), uint32(0o100444)
	if fi.IsDir() {
		kind, mode = 2, 0o40555
	}
	size := uint32(min(fi.Size(), 1<<32-1))
	t := uint32(fi.ModTime().Unix())
	out.u32(kind).u32(mode).u32(1).u32(0).u32(0).u32(size).u32(4096).u32(0).u32((size + 511) / 512).u32(1).u32(fileID(rel))
	for i := 0; i < 3; i++ {
		out.u32(t).u32(0)
	}
}

func (s *NFSServer) fh(rel string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.index[rel]
	if !ok {
		if len(s.paths) >= maxHandle {
			s.gen++
			s.paths, s.index = []string{""}, map[string]uint32{"": 0}
		}
		i = uint32(len(s.paths))
		s.paths = append(s.paths, rel)
		s.index[rel] = i
	}
	b := make([]byte, fhSize)
	binary.BigEndian.PutUint32(b, s.gen)
	binary.BigEndian.PutUint32(b[4:], i)
	return b
}

func (s *NFSServer) lookupFH(b []byte) (string, bool) {
	if len(b) != fhSize {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := binary.BigEndian.Uint32(b[4:])
	if binary.BigEndian.Uint32(b) != s.gen || uint64(i) >= uint64(len(s.paths)) {
		return "", false
	}
	return s.paths[i], true
}
