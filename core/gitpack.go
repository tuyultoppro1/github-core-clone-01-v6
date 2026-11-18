package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
)

// --- pkt-lines ----------------------------------------------------------

func pktLine(s string) []byte {
	n := len(s) + 4
	out := make([]byte, n)
	copy(out, []byte(fmt.Sprintf("%04x", n)))
	copy(out[4:], s)
	return out
}

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// pktLines parses a pkt-line stream into lines (payloads without the
// length prefix); flush -> "" marker lines are returned as "\x00flush",
// delim as "\x00delim". Raw binary content after the flush is not parsed.
type pktResult struct {
	lines []string
	rest  []byte // bytes after the first flush
}

func parsePktLines(b []byte) pktResult {
	var res pktResult
	i := 0
	for i+4 <= len(b) {
		var n int
		fmt.Sscanf(string(b[i:i+4]), "%04x", &n)
		if n == 0 {
			res.lines = append(res.lines, "\x00flush")
			res.rest = b[i+4:]
			return res
		}
		if n == 1 {
			res.lines = append(res.lines, "\x00delim")
			i += 4
			continue
		}
		if i+n > len(b) {
			break
		}
		res.lines = append(res.lines, string(b[i+4:i+n]))
		i += n
	}
	res.rest = b[i:]
	return res
}

// --- pack reading ---------------------------------------------------------

type packObject struct {
	Type string
	Data []byte
}

func packTypeName(t int) string {
	switch t {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	case 6:
		return "ofs_delta"
	case 7:
		return "ref_delta"
	}
	return ""
}

// readPack parses a packfile. It verifies each object's sha1 by recomputing
// it after delta resolution, and returns objects keyed by computed oid.
// Deltas are resolved against base objects already in the pack or the repo.
func readPack(repo *Repo, pack []byte) (map[string]*Object, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	br := bytes.NewReader(pack[12:])
	byOID := map[string]*Object{}
	type delta struct {
		oid  string
		base string // base oid (ref_delta) or "" (ofs_delta, resolved below)
		data []byte
	}
	var deltas []delta
	// ofs_delta bases are identified by negative offset from the entry start.
	type ofsEntry struct {
		oid string
	}
	_ = ofsEntry{}
	for i := 0; i < count; i++ {
		entryStart, _ := br.Seek(0, io.SeekCurrent)
		// varint: type in bits 4-6 of first byte, size low 4 bits + 7-bit groups.
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		typ := int((b >> 4) & 7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b, err = br.ReadByte()
			if err != nil {
				return nil, err
			}
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var baseOID string
		var baseOff int64 = -1
		switch typ {
		case 6: // ofs_delta
			b, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				b, err = br.ReadByte()
				if err != nil {
					return nil, err
				}
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			baseOff = entryStart - off
		case 7: // ref_delta
			raw := make([]byte, 20)
			if _, err := io.ReadFull(br, raw); err != nil {
				return nil, err
			}
			baseOID = hex.EncodeToString(raw)
		}
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		zr.Close()
		if int64(len(data)) != size {
			return nil, fmt.Errorf("pack entry size mismatch")
		}
		if typ == 6 || typ == 7 {
			// delta data: source size varint, target size varint, ops.
			deltas = append(deltas, delta{oid: "", base: baseOID, data: data})
			// remember offset for ofs_delta resolution
			deltas[len(deltas)-1].oid = fmt.Sprintf("ofs:%d", entryStart)
			if typ == 6 {
				// stash base offset in base field encoding
				deltas[len(deltas)-1].base = fmt.Sprintf("ofs:%d", baseOff)
			}
			continue
		}
		typName := packTypeName(typ)
		if typName == "" {
			return nil, fmt.Errorf("unknown pack type %d", typ)
		}
		oid := hashObject(typName, data)
		byOID[oid] = &Object{Type: typName, Data: data}
	}
	// Resolve deltas. Base may be in byOID, in the repo, or an earlier delta.
	// Build an offset -> oid map for ofs bases by re-walking: entries are
	// stored in pack order; ofs bases always point backwards, so a base
	// object is already materialised in byOID when we reach the delta.
	// We re-derive the offset->oid map from the pack itself by scanning
	// entry starts again, but only plain objects were recorded... simpler:
	// resolve deltas iteratively against byOID + repo objects.
	// For ofs_delta we need the base oid: re-scan the pack for entry starts.
	offToOID := map[int64]string{}
	{
		br2 := bytes.NewReader(pack[12:])
		for i := 0; i < count; i++ {
			entryStart, _ := br2.Seek(0, io.SeekCurrent)
			b, err := br2.ReadByte()
			if err != nil {
				break
			}
			typ := int((b >> 4) & 7)
			// skip size varint
			for b&0x80 != 0 {
				b, err = br2.ReadByte()
				if err != nil {
					break
				}
			}
			var baseOff int64 = -1
			var baseOID string
			if typ == 6 {
				b, err := br2.ReadByte()
				if err != nil {
					break
				}
				off := int64(b & 0x7f)
				for b&0x80 != 0 {
					b, err = br2.ReadByte()
					if err != nil {
						break
					}
					off = ((off + 1) << 7) | int64(b&0x7f)
				}
				baseOff = entryStart - off
			} else if typ == 7 {
				raw := make([]byte, 20)
				if _, err := io.ReadFull(br2, raw); err != nil {
					break
				}
				baseOID = hex.EncodeToString(raw)
			}
			zr, err := zlib.NewReader(br2)
			if err != nil {
				break
			}
			data, err := io.ReadAll(zr)
			if err != nil {
				break
			}
			zr.Close()
			if typ != 6 && typ != 7 {
				offToOID[entryStart] = hashObject(packTypeName(typ), data)
			} else if typ == 7 {
				offToOID[entryStart] = "" // resolved via baseOID
				_ = baseOID
			} else {
				_ = baseOff
			}
		}
	}
	_ = offToOID
	// Iterative delta resolution.
	for len(deltas) > 0 {
		progress := false
		rest := []delta{}
		for _, d := range deltas {
			var base *Object
			var baseOID string
			if strings.HasPrefix(d.base, "ofs:") {
				// ofs base: find the object at that pack offset. Re-scan
				// the pack to locate the entry at that offset and decode it
				// (its oid is in byOID if it was a plain object; if the
				// base itself is a delta it is resolved in a later pass).
				var off int64
				fmt.Sscanf(d.base, "ofs:%d", &off)
				oid, ok := offToOID[off]
				if ok && oid != "" {
					baseOID = oid
				} else {
					// base is itself a delta not yet resolved; defer
					rest = append(rest, d)
					continue
				}
			} else {
				baseOID = d.base
			}
			if o, ok := byOID[baseOID]; ok {
				base = o
			} else if repo != nil {
				base = repo.Objects[baseOID]
			}
			if base == nil {
				rest = append(rest, d)
				continue
			}
			data, err := applyDelta(base.Data, d.data)
			if err != nil {
				return nil, err
			}
			oid := hashObject(base.Type, data)
			byOID[oid] = &Object{Type: base.Type, Data: data}
			offToOID[parseOfsKey(d.oid)] = oid
			progress = true
		}
		deltas = rest
		if !progress {
			return nil, fmt.Errorf("unresolvable delta in pack")
		}
	}
	// Verify trailer checksum if present.
	if len(pack) >= 20 {
		sum := sha1.Sum(pack[:len(pack)-20])
		if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
			// The client (git) always writes a valid trailer; a mismatch
			// means a truncated/corrupt pack.
			return nil, fmt.Errorf("pack trailer checksum mismatch")
		}
	}
	return byOID, nil
}

func parseOfsKey(s string) int64 {
	var v int64
	fmt.Sscanf(s, "ofs:%d", &v)
	return v
}

// applyDelta applies a git delta to base data, returning the result.
func applyDelta(base, delta []byte) ([]byte, error) {
	// delta: varint source size, varint target size, ops.
	rd := bytes.NewReader(delta)
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			b, err := rd.ReadByte()
			if err != nil {
				return 0, err
			}
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for rd.Len() > 0 {
		op, err := rd.ReadByte()
		if err != nil {
			return nil, err
		}
		if op&0x80 != 0 {
			// copy from base
			var off, size int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					b, err := rd.ReadByte()
					if err != nil {
						return nil, err
					}
					off |= int64(b) << (8 * uint(i))
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					b, err := rd.ReadByte()
					if err != nil {
						return nil, err
					}
					size |= int64(b) << (8 * uint(i))
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out.Write(base[off : off+size])
		} else if op != 0 {
			n := int(op)
			buf := make([]byte, n)
			if _, err := io.ReadFull(rd, buf); err != nil {
				return nil, err
			}
			out.Write(buf)
		} else {
			return nil, fmt.Errorf("delta op 0")
		}
	}
	if int64(out.Len()) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	// The resulting type is the base object's type.
	return out.Bytes(), nil
}

// --- pack writing ---------------------------------------------------------

// writePack serialises objects as a pack v2 with plain (undeltified)
// entries, sorted by oid for determinism.
func writePack(repo *Repo, oids []string) []byte {
	sort.Strings(oids)
	var body bytes.Buffer
	// header
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(oids)))
	body.Write(hdr)
	for _, oid := range oids {
		o := repo.Objects[oid]
		if o == nil {
			continue
		}
		var typ int
		switch o.Type {
		case "commit":
			typ = 1
		case "tree":
			typ = 2
		case "blob":
			typ = 3
		case "tag":
			typ = 4
		}
		size := len(o.Data)
		// type+size varint
		var hdrBytes []byte
		first := byte((typ << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		hdrBytes = append(hdrBytes, first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			hdrBytes = append(hdrBytes, b)
		}
		body.Write(hdrBytes)
		zw, _ := zlib.NewWriterLevel(&body, zlib.DefaultCompression)
		zw.Write(o.Data)
		zw.Close()
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// --- fetch selection --------------------------------------------------------

// selectFetch computes the objects to send for a fetch.
// wants: oids the client wants; haves: oids the client has;
// depth: 0 = full history; newShallow/unshallow are returned when depth>0.
func selectFetch(repo *Repo, wants, haves []string, depth int, oldShallow []string) (send []string, newShallow, unshallow []string) {
	haveSet := map[string]bool{}
	for _, h := range haves {
		haveSet[h] = true
	}
	cutSet := map[string]bool{}
	for _, s := range oldShallow {
		cutSet[s] = true
	}
	if depth == 0 {
		wantReach := repo.reachable(wants)
		haveReach := repo.reachableFrom(haves, cutSet)
		for oid := range wantReach {
			if !haveReach[oid] {
				send = append(send, oid)
			}
		}
		sort.Strings(send)
		return send, nil, nil
	}
	// Shallow fetch: BFS by commit generation from wants.
	included := map[string]bool{} // commits included
	boundary := map[string]bool{} // commits that become shallow
	type item struct {
		oid string
		gen int
	}
	queue := []item{}
	seen := map[string]bool{}
	for _, w := range wants {
		c := repo.peelToCommit(w)
		if c == "" {
			// want is a tree/blob/tag-to-non-commit: include directly.
			if o := repo.Objects[w]; o != nil {
				included[w] = true
			}
			continue
		}
		queue = append(queue, item{c, 0})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] {
			continue
		}
		seen[it.oid] = true
		if haveSet[it.oid] {
			// Client already has this commit, but its parents may be
			// exactly what a deepen asks for: keep walking deeper.
			o := repo.Objects[it.oid]
			if o != nil && o.Type == "commit" {
				for _, p := range parseCommit(o.Data).Parents {
					queue = append(queue, item{p, it.gen + 1})
				}
			}
			continue
		}
		if it.gen >= depth {
			continue
		}
		included[it.oid] = true
		if it.gen == depth-1 {
			// Deepest included layer: parents are cut, so this commit
			// itself is the shallow boundary.
			boundary[it.oid] = true
			continue
		}
		o := repo.Objects[it.oid]
		if o != nil && o.Type == "commit" {
			for _, p := range parseCommit(o.Data).Parents {
				queue = append(queue, item{p, it.gen + 1})
			}
		}
	}
	// Objects to send: included commits + their trees/blobs, plus
	// non-commit wants, minus anything reachable from haves.
	sendSet := map[string]bool{}
	for oid := range included {
		o := repo.Objects[oid]
		if o == nil {
			continue
		}
		sendSet[oid] = true
		if o.Type == "commit" {
			tree := parseCommit(o.Data).Tree
			// walk this commit's tree (not its parents')
			var walkTree func(t string)
			walkTree = func(t string) {
				if sendSet[t] {
					return
				}
				to := repo.Objects[t]
				if to == nil || to.Type != "tree" {
					return
				}
				sendSet[t] = true
				for _, e := range parseTree(to.Data) {
					if e.Mode == "40000" {
						walkTree(e.OID)
					} else {
						sendSet[e.OID] = true
					}
				}
			}
			walkTree(tree)
		} else if o.Type == "tag" {
			// include tag target closure at same depth rules (rare)
			t := parseTag(o.Data).Object
			if tc := repo.peelToCommit(t); tc != "" {
				sendSet[t] = true
			}
		}
	}
	haveReach := repo.reachableFrom(haves, cutSet)
	for oid := range sendSet {
		if !haveReach[oid] {
			send = append(send, oid)
		}
	}
	sort.Strings(send)
	for oid := range boundary {
		if !haveSet[oid] {
			newShallow = append(newShallow, oid)
		}
	}
	sort.Strings(newShallow)
	// unshallow: old shallow boundaries now fully known (in included or
	// haves, with all parents included or haves).
	for _, s := range oldShallow {
		if haveSet[s] || included[s] {
			o := repo.Objects[s]
			complete := true
			if o != nil && o.Type == "commit" {
				for _, p := range parseCommit(o.Data).Parents {
					if !haveSet[p] && !included[p] {
						complete = false
					}
				}
			}
			if complete {
				unshallow = append(unshallow, s)
			}
		}
	}
	sort.Strings(unshallow)
	return send, newShallow, unshallow
}
