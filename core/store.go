package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Scenario constants (fixed by the competition).
const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

var (
	zeroOID = strings.Repeat("0", 40)
	// deterministic fake clock: starts at a fixed epoch, ticks one second
	// per mutation so two runs on the same instance export identical state.
	clockEpoch = int64(1763400000)
)

// --- Store ----------------------------------------------------------

type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo // key owner + "/" + name
	repoSeq   []*Repo          // creation order
	nextRepo  int              // next databaseId
	nextIssue int              // next global issue id
	nextCmt   int              // next global comment id
	nextPull  int              // next global pull id
	clock     int64            // fake-clock counter
}

func NewStore() *Store {
	return &Store{
		repos:     make(map[string]*Repo),
		nextRepo:  5000,
		nextIssue: 100000,
		nextCmt:   200000,
		nextPull:  300000,
		clock:     clockEpoch,
	}
}

// tick returns the next deterministic timestamp (RFC3339 UTC).
func (s *Store) tick() string {
	s.clock++
	return time.Unix(s.clock, 0).UTC().Format(time.RFC3339)
}

func (s *Store) findRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[owner+"/"+name]
}

func (s *Store) createRepo(name, desc string, priv bool) *Repo {
	r := &Repo{
		Owner:        viewerLogin,
		Name:         name,
		Description:  desc,
		Private:      priv,
		Default:      "main",
		DatabaseID:   s.nextRepo,
		Refs:         make(map[string]string),
		Objects:      make(map[string]*Object),
		Labels:       make(map[string]*Label),
		IssuesByNum:  make(map[int]*Issue),
		PullsByNum:   make(map[int]*Pull),
		NextIssueNum: 1,
		NextPullNum:  1,
	}
	s.nextRepo++
	s.repos[viewerLogin+"/"+name] = r
	s.repoSeq = append(s.repoSeq, r)
	return r
}

// --- Repo / issues / pulls -------------------------------------------

type Object struct {
	Type string
	Data []byte
}

type Repo struct {
	Owner        string
	Name         string
	Description  string
	Private      bool
	Default      string
	DatabaseID   int
	Refs         map[string]string // full refname -> oid
	Objects      map[string]*Object
	Issues       []*Issue
	IssuesByNum  map[int]*Issue
	NextIssueNum int
	Labels       map[string]*Label
	LabelOrder   []string
	Pulls        []*Pull
	PullsByNum   map[int]*Pull
	NextPullNum  int
}

type Label struct {
	Name  string
	Color string
}

type Issue struct {
	Number   int
	ID       int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*Comment
	Created  string
	Updated  string
	Seq      int
}

type Comment struct {
	ID      int
	Body    string
	Created string
}

type Pull struct {
	Number         int
	ID             int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	Created        string
	Seq            int
}

// --- Global IDs (opaque, stable, decodable) --------------------------

// idFor encodes kind + numeric id as base64url with a type prefix.
// GraphQL ids must not be plain databaseId/number.
func idFor(kind string, n int) string {
	raw := fmt.Sprintf("%s:%d", kind, n)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func parseGlobalID(s string) (kind string, n int, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", 0, false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	var v int
	if _, err := fmt.Sscanf(parts[1], "%d", &v); err != nil {
		return "", 0, false
	}
	return parts[0], v, true
}

// --- Object helpers ---------------------------------------------------

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Repo) putObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.Objects[oid] = &Object{Type: typ, Data: cp}
	}
	return oid
}

func (r *Repo) getObject(oid string) *Object {
	return r.Objects[oid]
}

// --- Commit / tree / tag parsing ---------------------------------------

type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) *Commit {
	c := &Commit{}
	body := data
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		c.Message = string(data[i+2:])
		body = data[:i]
	}
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "committer "):
			c.Committer = strings.TrimSpace(line[10:])
		}
	}
	return c
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		j := i + sp + 1
		nul := bytes.IndexByte(data[j:], 0)
		if nul < 0 {
			break
		}
		name := string(data[j : j+nul])
		oidStart := j + nul + 1
		if oidStart+20 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		i = oidStart + 20
	}
	return out
}

func buildTree(entries []TreeEntry) []byte {
	// git sorts tree entries by name, dirs as name+"/".
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(a, b int) bool {
		na, nb := sorted[a].Name, sorted[b].Name
		if sorted[a].Mode == "40000" {
			na += "/"
		}
		if sorted[b].Mode == "40000" {
			nb += "/"
		}
		return na < nb
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type Tag struct {
	Object string
	Type   string
	Tag    string
	Tagger string
}

func parseTag(data []byte) *Tag {
	t := &Tag{}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			t.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			t.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			t.Tagger = strings.TrimSpace(line[7:])
		}
		if line == "" {
			break
		}
	}
	return t
}

// peelToCommit resolves an oid through tags to a commit oid.
func (r *Repo) peelToCommit(oid string) string {
	for {
		o := r.Objects[oid]
		if o == nil {
			return ""
		}
		if o.Type == "commit" {
			return oid
		}
		if o.Type == "tag" {
			oid = parseTag(o.Data).Object
			continue
		}
		return ""
	}
}

// isAncestor reports whether anc is an ancestor of (or equal to) oid.
func (r *Repo) isAncestor(anc, oid string) bool {
	if anc == oid {
		return true
	}
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.Objects[cur]
		if o == nil || o.Type != "commit" {
			continue
		}
		c := parseCommit(o.Data)
		queue = append(queue, c.Parents...)
	}
	return false
}

// reachable returns every object reachable from roots (commits, tags,
// trees, blobs), following commit->parent/tree, tag->object, tree->entries.
func (r *Repo) reachable(roots []string) map[string]bool {
	return r.reachableFrom(roots, nil)
}

// reachableFrom is reachable, except traversal stops at the parents of
// commits in cut: a shallow boundary commit is one the client has, but
// whose history it does not. The boundary commit itself and its tree
// are still included.
func (r *Repo) reachableFrom(roots []string, cut map[string]bool) map[string]bool {
	out := map[string]bool{}
	var walk func(oid string)
	walk = func(oid string) {
		if oid == "" || out[oid] {
			return
		}
		o := r.Objects[oid]
		if o == nil {
			return
		}
		out[oid] = true
		switch o.Type {
		case "commit":
			c := parseCommit(o.Data)
			walk(c.Tree)
			if !cut[oid] {
				for _, p := range c.Parents {
					walk(p)
				}
			}
		case "tag":
			walk(parseTag(o.Data).Object)
		case "tree":
			for _, e := range parseTree(o.Data) {
				walk(e.OID)
			}
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

// treeAt returns the tree oid of a commit ("" if not a commit).
func (r *Repo) treeAt(oid string) string {
	o := r.Objects[oid]
	if o == nil || o.Type != "commit" {
		return ""
	}
	return parseCommit(o.Data).Tree
}

// flattenTree returns path -> (mode, oid, isDir) for every entry.
type flatEntry struct {
	Mode string
	OID  string
	Dir  bool
}

func (r *Repo) flattenTree(treeOID string, prefix string, out map[string]flatEntry) {
	o := r.Objects[treeOID]
	if o == nil || o.Type != "tree" {
		return
	}
	for _, e := range parseTree(o.Data) {
		path := prefix + e.Name
		if e.Mode == "40000" {
			out[path] = flatEntry{Mode: e.Mode, OID: e.OID, Dir: true}
			r.flattenTree(e.OID, path+"/", out)
		} else {
			out[path] = flatEntry{Mode: e.Mode, OID: e.OID}
		}
	}
}
