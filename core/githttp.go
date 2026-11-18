package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// --- git HTTP routing -----------------------------------------------------

func handleGit(w http.ResponseWriter, r *http.Request) {
	// Path: /<owner>/<repo>.git/<rest>
	p := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		writeErr(w, 404, "Not Found")
		return
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	rest := parts[2]
	store.mu.RLock()
	repo := store.findRepo(owner, repoName)
	store.mu.RUnlock()
	if repo == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	switch {
	case rest == "info/refs" && r.Method == "GET":
		handleInfoRefs(w, r, repo)
	case rest == "git-upload-pack" && r.Method == "POST":
		handleUploadPack(w, r, repo)
	case rest == "git-receive-pack" && r.Method == "POST":
		handleReceivePack(w, r, repo)
	default:
		writeErr(w, 404, "Not Found")
	}
}

// --- advertisements ---------------------------------------------------------

func sortedRefs(repo *Repo) []string {
	names := make([]string, 0, len(repo.Refs))
	for n := range repo.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// refLinesV0 builds the v0 ref advertisement lines (without the
// "# service=..." preamble). Caps go on the first line.
func refLinesV0(repo *Repo, caps string) []string {
	var lines []string
	names := sortedRefs(repo)
	if len(names) == 0 {
		return []string{zeroOID + " capabilities^{}\x00" + caps + "\n"}
	}
	defTip := repo.Refs["refs/heads/"+repo.Default]
	first := true
	emit := func(oid, ref string) {
		if first {
			lines = append(lines, oid+" "+ref+"\x00"+caps+"\n")
			first = false
		} else {
			lines = append(lines, oid+" "+ref+"\n")
		}
	}
	if defTip != "" {
		emit(defTip, "HEAD")
	}
	for _, name := range names {
		oid := repo.Refs[name]
		emit(oid, name)
		// peeled tag line for annotated tags
		if strings.HasPrefix(name, "refs/tags/") {
			if o := repo.Objects[oid]; o != nil && o.Type == "tag" {
				peeled := repo.peelToCommit(oid)
				if peeled != "" {
					lines = append(lines, peeled+" "+name+"^{}\n")
				}
			}
		}
	}
	return lines
}

func handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	svc := r.URL.Query().Get("service")
	store.mu.RLock()
	defer store.mu.RUnlock()
	switch svc {
	case "git-upload-pack":
		var buf bytes.Buffer
		buf.Write(pktLine("# service=git-upload-pack\n"))
		buf.Write(pktFlush())
		if r.Header.Get("Git-Protocol") == "version=2" {
			buf.Write(pktLine("version 2\n"))
			buf.Write(pktLine("ls-refs=unborn\n"))
			buf.Write(pktLine("fetch=shallow\n"))
			buf.Write(pktLine("agent=gitforge-v6\n"))
			buf.Write(pktLine("object-format=sha1\n"))
			buf.Write(pktFlush())
		} else {
			caps := "symref=HEAD:refs/heads/" + repo.Default + " side-band-64k thin-pack ofs-delta shallow multi_ack agent"
			for _, l := range refLinesV0(repo, caps) {
				buf.Write(pktLine(l))
			}
			buf.Write(pktFlush())
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(200)
		w.Write(buf.Bytes())
	case "git-receive-pack":
		var buf bytes.Buffer
		buf.Write(pktLine("# service=git-receive-pack\n"))
		buf.Write(pktFlush())
		caps := "report-status side-band-64k delete-refs ofs-delta agent"
		for _, l := range refLinesV0(repo, caps) {
			buf.Write(pktLine(l))
		}
		buf.Write(pktFlush())
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.WriteHeader(200)
		w.Write(buf.Bytes())
	default:
		writeErr(w, 404, "Not Found")
	}
}

// --- upload-pack -------------------------------------------------------------

func handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) < 4 {
		writeErr(w, 400, "bad request")
		return
	}
	// v2 if the first line starts with "command=".
	first := parsePktLines(body)
	if len(first.lines) > 0 && strings.HasPrefix(first.lines[0], "command=") {
		handleUploadPackV2(w, r, repo, body)
		return
	}
	handleUploadPackV0Real(w, r, repo, body)
}

// sidebandPack wraps pack bytes in sideband band-1 packets.
func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	const chunk = 32768
	for len(pack) > 0 {
		n := chunk
		if len(pack) < n {
			n = len(pack)
		}
		payload := append([]byte{1}, pack[:n]...)
		buf.Write(pktLine(string(payload)))
		pack = pack[n:]
	}
	buf.Write(pktFlush())
	return buf.Bytes()
}

func handleUploadPackV2(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	// Parse the whole request as pkt-lines.
	pr := parsePktLines(body)
	var cmd string
	var wants, haves, shallowList []string
	depth := 0
	var prefixes []string
	peel := false
	unborn := false
	for _, line := range pr.lines {
		if line == "\x00flush" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "command="):
			cmd = strings.TrimSuffix(line[len("command="):], "\n")
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallowList = append(shallowList, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			fmt.Sscanf(line, "deepen %d", &depth)
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(line[len("ref-prefix "):]))
		case strings.HasPrefix(line, "peel"):
			peel = true
		case strings.HasPrefix(line, "unborn"):
			unborn = true
		}
	}
	_ = unborn
	store.mu.RLock()
	defer store.mu.RUnlock()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	switch cmd {
	case "ls-refs":
		var buf bytes.Buffer
		emit := func(oid, ref string, extra string) {
			if len(prefixes) > 0 {
				ok := false
				for _, p := range prefixes {
					if strings.HasPrefix(ref, p) {
						ok = true
					}
				}
				if !ok {
					return
				}
			}
			buf.Write(pktLine(oid + " " + ref + extra + "\n"))
		}
		defTip := repo.Refs["refs/heads/"+repo.Default]
		if len(prefixes) == 0 {
			if defTip != "" {
				emit(defTip, "HEAD", " symref-target:refs/heads/"+repo.Default)
			} else {
				emit("unborn", "HEAD", " symref-target:refs/heads/"+repo.Default)
			}
		}
		for _, name := range sortedRefs(repo) {
			oid := repo.Refs[name]
			extra := ""
			if peel && strings.HasPrefix(name, "refs/tags/") {
				if o := repo.Objects[oid]; o != nil && o.Type == "tag" {
					if p := repo.peelToCommit(oid); p != "" {
						extra = " peeled:" + p
					}
				}
			}
			emit(oid, name, extra)
		}
		buf.Write(pktFlush())
		w.WriteHeader(200)
		w.Write(buf.Bytes())
	case "fetch":
		pack, newShallow, unshallow := fetchPack(repo, wants, haves, depth, shallowList)
		var buf bytes.Buffer
		buf.Write(pktLine("acknowledgments\n"))
		if len(haves) == 0 {
			buf.Write(pktLine("NAK\n"))
		} else {
			for _, h := range haves {
				if repo.Objects[h] != nil {
					buf.Write(pktLine("ACK " + h + "\n"))
				}
			}
			buf.Write(pktLine("ready\n"))
		}
		buf.Write(pktDelim())
		if len(newShallow) > 0 || len(unshallow) > 0 {
			buf.Write(pktLine("shallow-info\n"))
			for _, s := range newShallow {
				buf.Write(pktLine("shallow " + s + "\n"))
			}
			for _, s := range unshallow {
				buf.Write(pktLine("unshallow " + s + "\n"))
			}
			buf.Write(pktDelim())
		}
		buf.Write(pktLine("packfile\n"))
		buf.Write(sidebandPack(pack))
		w.WriteHeader(200)
		w.Write(buf.Bytes())
	default:
		writeErr(w, 400, "unknown command")
	}
}

// --- upload-pack v0 ----------------------------------------------------------

func handleUploadPackV0Real(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	pr := parsePktLines(body)
	var wants, haves, shallowList []string
	depth := 0
	// lines until first flush are wants (+caps on first line); after the
	// flush come shallow/deepen lines, then another flush, then haves.
	phase := 0
	for _, line := range pr.lines {
		if line == "\x00flush" {
			phase++
			continue
		}
		switch phase {
		case 0:
			if strings.HasPrefix(line, "want ") {
				rest := strings.TrimSpace(line[5:])
				if i := strings.Index(rest, " "); i >= 0 {
					rest = rest[:i]
				}
				wants = append(wants, rest)
			}
		case 1:
			if strings.HasPrefix(line, "shallow ") {
				shallowList = append(shallowList, strings.TrimSpace(line[8:]))
			} else if strings.HasPrefix(line, "deepen ") {
				fmt.Sscanf(line, "deepen %d", &depth)
			}
		default:
			if strings.HasPrefix(line, "have ") {
				haves = append(haves, strings.TrimSpace(line[5:]))
			}
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	pack, newShallow, unshallow := fetchPack(repo, wants, haves, depth, shallowList)
	var buf bytes.Buffer
	if len(haves) == 0 {
		buf.Write(pktLine("NAK\n"))
	} else {
		for _, h := range haves {
			if repo.Objects[h] != nil {
				buf.Write(pktLine("ACK " + h + "\n"))
			}
		}
	}
	for _, s := range newShallow {
		buf.Write(pktLine("shallow " + s + "\n"))
	}
	for _, s := range unshallow {
		buf.Write(pktLine("unshallow " + s + "\n"))
	}
	buf.Write(sidebandPack(pack))
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	w.Write(buf.Bytes())
}

// --- receive-pack --------------------------------------------------------------

type pushCmd struct {
	oldOID string
	newOID string
	ref    string
}

func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	pr := parsePktLines(body)
	var cmds []pushCmd
	for _, line := range pr.lines {
		if line == "\x00flush" {
			break
		}
		// strip capabilities after NUL on the first line
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSuffix(line, "\n")
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		cmds = append(cmds, pushCmd{oldOID: f[0], newOID: f[1], ref: f[2]})
	}
	packBytes := pr.rest
	store.mu.Lock()
	defer store.mu.Unlock()
	var report bytes.Buffer
	// The report is sideband-framed (band 1) because side-band-64k was
	// negotiated: each line is a pkt-line whose payload starts with 0x01.
	report.Write(pktLine("\x01unpack ok\n"))
	if len(packBytes) > 0 {
		// A pack follows the command flush. Parse it; on failure the
		// whole push is refused with ng lines (git reports unpack error).
		objs, perr := readPack(repo, packBytes)
		if perr != nil {
			var rep2 bytes.Buffer
			rep2.Write(pktLine("\x01unpack " + perr.Error() + "\n"))
			for _, c := range cmds {
				rep2.Write(pktLine("\x01ng " + c.ref + " unpack failed\n"))
			}
			rep2.Write(pktFlush())
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			w.WriteHeader(200)
			w.Write(rep2.Bytes())
			return
		}
		for oid, o := range objs {
			if _, ok := repo.Objects[oid]; !ok {
				cp := make([]byte, len(o.Data))
				copy(cp, o.Data)
				repo.Objects[oid] = &Object{Type: o.Type, Data: cp}
			}
		}
	}
	for _, c := range cmds {
		ok, reason := applyPush(repo, c)
		if ok {
			report.Write(pktLine("\x01ok " + c.ref + "\n"))
		} else {
			report.Write(pktLine("\x01ng " + c.ref + " " + reason + "\n"))
		}
	}
	report.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	w.Write(report.Bytes())
}

// applyPush validates and applies one ref update. Returns (ok, reason).
func applyPush(repo *Repo, c pushCmd) (bool, string) {
	cur, exists := repo.Refs[c.ref]
	if c.newOID == zeroOID {
		// delete
		if !exists {
			return false, "remote ref does not exist"
		}
		if c.oldOID != zeroOID && c.oldOID != cur {
			return false, "non-fast-forward"
		}
		delete(repo.Refs, c.ref)
		repo.bumpGen()
		return true, ""
	}
	// create or update: new object must exist and resolve to a commit
	// (for refs/heads/*) or any object (for tags).
	newCommit := repo.peelToCommit(c.newOID)
	if newCommit == "" {
		// tag refs may point at non-commit objects; heads must not.
		if strings.HasPrefix(c.ref, "refs/heads/") {
			return false, "object not found"
		}
		if repo.Objects[c.newOID] == nil {
			return false, "object not found"
		}
	}
	if !exists {
		// create
		if c.oldOID != zeroOID {
			return false, "non-fast-forward"
		}
		repo.Refs[c.ref] = c.newOID
		repo.bumpGen()
		return true, ""
	}
	// update: old must match current
	if c.oldOID != cur {
		return false, "non-fast-forward"
	}
	if newCommit != "" && !repo.isAncestor(cur, newCommit) && cur != newCommit {
		return false, "non-fast-forward"
	}
	repo.Refs[c.ref] = c.newOID
	repo.bumpGen()
	return true, ""
}
