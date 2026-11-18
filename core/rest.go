package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- REST v3 ---------------------------------------------------------------

func handleREST(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v3")
	p = strings.TrimPrefix(p, "/")
	segs := []string{}
	if p != "" {
		segs = strings.Split(p, "/")
	}
	// read body as raw for JSON error detection
	var body map[string]any
	var bodyErr error
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE" {
		var raw []byte
		raw, _ = readAllBody(r)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
		}
		if body == nil {
			body = map[string]any{}
		}
	}
	_ = bodyErr
	switch {
	case len(segs) == 0 && r.Method == "GET":
		w.Header().Set("X-OAuth-Scopes", "repo, user")
		writeJSON(w, 200, map[string]any{
			"current_user_url": "https://api.github.com/user",
			"repository_url":   "https://api.github.com/repos/{owner}/{repo}",
		})
	case len(segs) == 1 && segs[0] == "meta" && r.Method == "GET":
		writeJSON(w, 200, map[string]any{"installed_version": "1.0.0"})
	case len(segs) == 1 && segs[0] == "user" && r.Method == "GET":
		writeJSON(w, 200, userJSON())
	case len(segs) == 2 && segs[0] == "user" && segs[1] == "repos":
		if r.Method == "GET" {
			handleListUserRepos(w, r)
		} else if r.Method == "POST" {
			handleCreateRepo(w, r, body)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(segs) == 2 && segs[0] == "repos":
		writeErr(w, 404, "Not Found")
	case len(segs) >= 3 && segs[0] == "repos":
		handleRepoRoutes(w, r, segs, body)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf []byte
	tmp := make([]byte, 8192)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

func userJSON() map[string]any {
	return map[string]any{
		"login": viewerLogin,
		"id":    viewerID,
		"name":  viewerName,
		"email": viewerEmail,
		"type":  "User",
	}
}

func repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"id":             r.DatabaseID,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]any{"login": r.Owner, "id": viewerID, "type": "User"},
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": r.Default,
		"html_url":       "https://github.com/" + r.Owner + "/" + r.Name,
		"url":            "https://api.github.com/repos/" + r.Owner + "/" + r.Name,
		"created_at":     "2026-11-18T00:00:00Z",
		"updated_at":     "2026-11-18T00:00:00Z",
		"has_issues":     true,
		"fork":           false,
	}
}

func handleListUserRepos(w http.ResponseWriter, r *http.Request) {
	perPage := queryInt(r, "per_page", 30)
	page := queryInt(r, "page", 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	total := len(store.repoSeq)
	start := (page - 1) * perPage
	if start >= total {
		writeJSON(w, 200, []any{})
		return
	}
	end := start + perPage
	if end > total {
		end = total
	}
	out := make([]any, 0, end-start)
	for _, repo := range store.repoSeq[start:end] {
		out = append(out, repoJSON(repo))
	}
	if end < total {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		q.Set("per_page", strconv.Itoa(perPage))
		next := "http://" + r.Host + "/api/v3/user/repos?" + q.Encode()
		w.Header().Set("Link", "<"+next+">; rel=\"next\"")
	}
	writeJSON(w, 200, out)
}

func handleCreateRepo(w http.ResponseWriter, r *http.Request, body map[string]any) {
	name, _ := body["name"].(string)
	if name == "" {
		writeErr(w, 422, "name is required")
		return
	}
	desc, _ := body["description"].(string)
	priv, _ := body["private"].(bool)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.findRepo(viewerLogin, name) != nil {
		writeErr(w, 422, "Repository already exists")
		return
	}
	repo := store.createRepo(name, desc, priv)
	writeJSON(w, 201, repoJSON(repo))
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func strField(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

// --- repo-scoped routes ------------------------------------------------------

func handleRepoRoutes(w http.ResponseWriter, r *http.Request, segs []string, body map[string]any) {
	owner, name := segs[1], segs[2]
	rest := segs[3:]
	store.mu.RLock()
	repo := store.findRepo(owner, name)
	store.mu.RUnlock()
	if repo == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	switch {
	case len(rest) == 0:
		switch r.Method {
		case "GET":
			store.mu.RLock()
			defer store.mu.RUnlock()
			writeJSON(w, 200, repoJSON(repo))
		case "PATCH":
			store.mu.Lock()
			defer store.mu.Unlock()
			if d, ok := body["description"].(string); ok {
				repo.Description = d
			}
			if p, ok := body["private"].(bool); ok {
				repo.Private = p
			}
			writeJSON(w, 200, repoJSON(repo))
		case "DELETE":
			store.mu.Lock()
			defer store.mu.Unlock()
			delete(store.repos, owner+"/"+name)
			for i, rp := range store.repoSeq {
				if rp == repo {
					store.repoSeq = append(store.repoSeq[:i], store.repoSeq[i+1:]...)
					break
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(204)
		default:
			writeErr(w, 404, "Not Found")
		}
	case len(rest) == 2 && rest[0] == "git" && rest[1] == "refs" && r.Method == "POST":
		handleCreateRef(w, r, repo, body)
	case len(rest) == 4 && rest[0] == "git" && rest[1] == "refs" && rest[2] == "heads" && r.Method == "GET":
		store.mu.RLock()
		defer store.mu.RUnlock()
		branch := rest[3]
		oid, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{
			"ref":    "refs/heads/" + branch,
			"object": map[string]any{"sha": oid, "type": "commit"},
		})
	case len(rest) == 1 && rest[0] == "branches" && r.Method == "GET":
		handleListBranches(w, r, repo)
	case len(rest) == 2 && rest[0] == "branches" && r.Method == "GET":
		store.mu.RLock()
		defer store.mu.RUnlock()
		oid, ok := repo.Refs["refs/heads/"+rest[1]]
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, branchJSON(rest[1], oid))
	case len(rest) == 1 && rest[0] == "commits" && r.Method == "GET":
		handleListCommits(w, r, repo)
	case len(rest) >= 2 && rest[0] == "contents":
		handleContents(w, r, repo, rest[1:], body)
	case len(rest) == 1 && rest[0] == "labels" && r.Method == "POST":
		handleCreateLabel(w, r, repo, body)
	case len(rest) == 1 && rest[0] == "issues":
		if r.Method == "POST" {
			handleCreateIssue(w, r, repo, body)
		} else if r.Method == "GET" {
			handleListIssues(w, r, repo)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(rest) == 2 && rest[0] == "issues":
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			writeErr(w, 404, "Not Found")
			return
		}
		if r.Method == "GET" {
			handleGetIssue(w, r, repo, n)
		} else if r.Method == "PATCH" {
			handlePatchIssue(w, r, repo, n, body)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(rest) == 3 && rest[0] == "issues" && rest[2] == "comments":
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			writeErr(w, 404, "Not Found")
			return
		}
		if r.Method == "POST" {
			handleCreateComment(w, r, repo, n, body)
		} else if r.Method == "GET" {
			handleListComments(w, r, repo, n)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(rest) == 1 && rest[0] == "pulls":
		if r.Method == "POST" {
			handleCreatePull(w, r, repo, body)
		} else if r.Method == "GET" {
			handleListPulls(w, r, repo)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(rest) == 2 && rest[0] == "pulls":
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			writeErr(w, 404, "Not Found")
			return
		}
		if r.Method == "GET" {
			handleGetPull(w, r, repo, n)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "files" && r.Method == "GET":
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			writeErr(w, 404, "Not Found")
			return
		}
		handlePullFiles(w, r, repo, n)
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "merge" && r.Method == "PUT":
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			writeErr(w, 404, "Not Found")
			return
		}
		handleMergePull(w, r, repo, n, body)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func branchJSON(name, sha string) map[string]any {
	return map[string]any{
		"name":   name,
		"commit": map[string]any{"sha": sha},
	}
}

func handleListBranches(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var names []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, branchJSON(n, repo.Refs["refs/heads/"+n]))
	}
	writeJSON(w, 200, out)
}

func handleCreateRef(w http.ResponseWriter, r *http.Request, repo *Repo, body map[string]any) {
	ref, _ := body["ref"].(string)
	sha, _ := body["sha"].(string)
	if ref == "" || sha == "" {
		writeErr(w, 422, "ref and sha are required")
		return
	}
	if !strings.HasPrefix(ref, "refs/") {
		writeErr(w, 422, "ref must start with refs/")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if repo.Objects[sha] == nil {
		writeErr(w, 422, "Object does not exist")
		return
	}
	if _, ok := repo.Refs[ref]; ok {
		writeErr(w, 422, "Reference already exists")
		return
	}
	repo.Refs[ref] = sha
	typ := repo.Objects[sha].Type
	writeJSON(w, 201, map[string]any{
		"ref":    ref,
		"object": map[string]any{"sha": sha, "type": typ},
	})
}

func handleListCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	sha := r.URL.Query().Get("sha")
	store.mu.RLock()
	defer store.mu.RUnlock()
	var tip string
	if sha == "" {
		tip = repo.Refs["refs/heads/"+repo.Default]
	} else if strings.HasPrefix(sha, "refs/") {
		tip = repo.Refs[sha]
	} else if oid, ok := repo.Refs["refs/heads/"+sha]; ok {
		tip = oid
	} else if repo.Objects[sha] != nil {
		tip = sha
	}
	out := []any{}
	seen := map[string]bool{}
	cur := tip
	for cur != "" && !seen[cur] {
		seen[cur] = true
		o := repo.Objects[cur]
		if o == nil || o.Type != "commit" {
			break
		}
		c := parseCommit(o.Data)
		out = append(out, commitJSON(cur, c))
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	writeJSON(w, 200, out)
}

func commitJSON(sha string, c *Commit) map[string]any {
	author := parseIdent(c.Author)
	committer := parseIdent(c.Committer)
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"message":   c.Message,
			"author":    author,
			"committer": committer,
		},
		"author":    map[string]any{"login": viewerLogin, "id": viewerID},
		"committer": map[string]any{"login": viewerLogin, "id": viewerID},
	}
}

func parseIdent(s string) map[string]any {
	// "Name <email> 1234567890 +0000"
	name := s
	email := ""
	date := ""
	if i := strings.Index(s, "<"); i >= 0 {
		name = strings.TrimSpace(s[:i])
		rest := s[i+1:]
		if j := strings.Index(rest, ">"); j >= 0 {
			email = rest[:j]
			tail := strings.TrimSpace(rest[j+1:])
			if f := strings.Fields(tail); len(f) >= 1 {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					date = time.Unix(n, 0).UTC().Format(time.RFC3339)
				}
			}
		}
	}
	return map[string]any{"name": name, "email": email, "date": date}
}

// --- deterministic clock ---------------------------------------------------

func (s *Store) tickEpoch() (int64, string) {
	s.clock++
	return s.clock, time.Unix(s.clock, 0).UTC().Format(time.RFC3339)
}

func (s *Store) commitOnBranch(repo *Repo, branch, message, treeOID string) string {
	epoch, _ := s.tickEpoch()
	author := fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, epoch)
	var parents []string
	if tip, ok := repo.Refs["refs/heads/"+branch]; ok {
		parents = []string{tip}
	}
	data := buildCommit(treeOID, parents, author, author, message)
	oid := repo.putObject("commit", data)
	repo.Refs["refs/heads/"+branch] = oid
	repo.bumpGen()
	return oid
}

// --- tree rebuild from a flat map -------------------------------------------

func (r *Repo) treeFromFlat(flat map[string]flatEntry) string {
	return r.treeFromFlatAt(flat, "")
}

func (r *Repo) treeFromFlatAt(flat map[string]flatEntry, prefix string) string {
	var entries []TreeEntry
	seenDir := map[string]bool{}
	for path, fe := range flat {
		if fe.Dir {
			continue // dirs are derived from file paths, never stored
		}
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			dir := rest[:i]
			if seenDir[dir] {
				continue
			}
			seenDir[dir] = true
			sub := r.treeFromFlatAt(flat, prefix+dir+"/")
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: sub})
		} else {
			entries = append(entries, TreeEntry{Mode: fe.Mode, Name: rest, OID: fe.OID})
		}
	}
	return r.putObject("tree", buildTree(entries))
}

// --- contents -----------------------------------------------------------------

func resolveRefTree(repo *Repo, ref string) (treeOID string, ok bool) {
	if ref == "" {
		ref = repo.Default
	}
	if tip, exists := repo.Refs["refs/heads/"+ref]; exists {
		return repo.treeAt(tip), true
	}
	if o := repo.Objects[ref]; o != nil && o.Type == "commit" {
		return repo.treeAt(ref), true
	}
	return "", false
}

func handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string, body map[string]any) {
	path := strings.Join(rest, "/")
	switch r.Method {
	case "GET":
		store.mu.RLock()
		defer store.mu.RUnlock()
		treeOID, ok := resolveRefTree(repo, r.URL.Query().Get("ref"))
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		// Walk to the path.
		curTree := treeOID
		segs := strings.Split(path, "/")
		for i, seg := range segs {
			o := repo.Objects[curTree]
			if o == nil || o.Type != "tree" {
				writeErr(w, 404, "Not Found")
				return
			}
			var found *TreeEntry
			for _, e := range parseTree(o.Data) {
				if e.Name == seg {
					ee := e
					found = &ee
					break
				}
			}
			if found == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			last := i == len(segs)-1
			if found.Mode == "40000" {
				if last {
					writeJSON(w, 200, dirListing(repo, found.OID, path))
					return
				}
				curTree = found.OID
				continue
			}
			if !last {
				writeErr(w, 404, "Not Found")
				return
			}
			blob := repo.Objects[found.OID]
			if blob == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64", "size": len(blob.Data),
				"name": seg, "path": path, "sha": found.OID,
				"content": base64.StdEncoding.EncodeToString(blob.Data),
			})
			return
		}
		// empty path (should not happen) -> root listing
		writeJSON(w, 200, dirListing(repo, treeOID, ""))
	case "PUT":
		handlePutContents(w, r, repo, path, body)
	case "DELETE":
		handleDeleteContents(w, r, repo, path, body)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func dirListing(repo *Repo, treeOID, prefix string) []any {
	o := repo.Objects[treeOID]
	out := []any{}
	if o == nil || o.Type != "tree" {
		return out
	}
	for _, e := range parseTree(o.Data) {
		typ := "file"
		size := 0
		if e.Mode == "40000" {
			typ = "dir"
		} else if b := repo.Objects[e.OID]; b != nil {
			size = len(b.Data)
		}
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		out = append(out, map[string]any{
			"type": typ, "name": e.Name, "path": p, "sha": e.OID, "size": size,
		})
	}
	return out
}

func handlePutContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string, body map[string]any) {
	message := strField(body, "message")
	contentB64 := strField(body, "content")
	branch := strField(body, "branch")
	if branch == "" {
		branch = repo.Default
	}
	sha := strField(body, "sha")
	raw, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(contentB64)
		if err != nil {
			writeErr(w, 400, "content is not valid base64")
			return
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	refName := "refs/heads/" + branch
	_, refExists := repo.Refs[refName]
	// A PUT to a branch that does not exist creates it from the default
	// branch (GitHub behaviour; the MCP fixture relies on it).
	flat := map[string]flatEntry{}
	if refExists {
		repo.flattenTree(repo.treeAt(repo.Refs[refName]), "", flat)
	} else if tip, ok := repo.Refs["refs/heads/"+repo.Default]; ok {
		repo.flattenTree(repo.treeAt(tip), "", flat)
	}
	created := true
	if fe, ok := flat[path]; ok && !fe.Dir {
		created = false
		if sha != "" && sha != fe.OID {
			writeErr(w, 409, "sha does not match")
			return
		}
	}
	blobOID := repo.putObject("blob", raw)
	flat[path] = flatEntry{Mode: "100644", OID: blobOID}
	newTree := repo.treeFromFlat(flat)
	commitOID := store.commitOnBranch(repo, branch, message, newTree)
	status := 201
	if !created {
		status = 200
	}
	writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"name": path[strings.LastIndex(path, "/")+1:], "path": path,
			"sha": blobOID, "size": len(raw), "type": "file", "encoding": "base64",
		},
		"commit": map[string]any{"sha": commitOID},
	})
}

func handleDeleteContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string, body map[string]any) {
	message := strField(body, "message")
	branch := strField(body, "branch")
	if branch == "" {
		branch = repo.Default
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	refName := "refs/heads/" + branch
	tip, ok := repo.Refs[refName]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	flat := map[string]flatEntry{}
	repo.flattenTree(repo.treeAt(tip), "", flat)
	if _, ok := flat[path]; !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	delete(flat, path)
	newTree := repo.treeFromFlat(flat)
	commitOID := store.commitOnBranch(repo, branch, message, newTree)
	writeJSON(w, 200, map[string]any{"commit": map[string]any{"sha": commitOID}})
}

// --- labels ---------------------------------------------------------------------

func handleCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo, body map[string]any) {
	name := strField(body, "name")
	color := strField(body, "color")
	if name == "" {
		writeErr(w, 422, "name is required")
		return
	}
	if color == "" {
		color = "ededed"
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if repo.Labels == nil {
		repo.Labels = map[string]*Label{}
	}
	if _, ok := repo.Labels[name]; ok {
		writeErr(w, 422, "Label already exists")
		return
	}
	repo.Labels[name] = &Label{Name: name, Color: color}
	repo.LabelOrder = append(repo.LabelOrder, name)
	writeJSON(w, 201, map[string]any{"name": name, "color": color})
}

// --- issues -----------------------------------------------------------------------

// Struct-encoded issue JSON: the read/write workloads hit this path
// thousands of times, and map-based encoding is several times slower.
type labelOut struct {
	Name string `json:"name"`
}

type userOut struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type issueOut struct {
	ID        int        `json:"id"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Labels    []labelOut `json:"labels"`
	User      userOut    `json:"user"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
	Comments  int        `json:"comments"`
}

func issueJSON(iss *Issue) issueOut {
	labels := make([]labelOut, 0, len(iss.Labels))
	for _, n := range iss.Labels {
		labels = append(labels, labelOut{Name: n})
	}
	return issueOut{
		ID: iss.ID, Number: iss.Number, Title: iss.Title, Body: iss.Body,
		State: iss.State, Labels: labels,
		User:      userOut{Login: viewerLogin, ID: viewerID},
		CreatedAt: iss.Created, UpdatedAt: iss.Updated,
		Comments: len(iss.Comments),
	}
}

func handleCreateIssue(w http.ResponseWriter, r *http.Request, repo *Repo, body map[string]any) {
	title := strField(body, "title")
	if title == "" {
		writeErr(w, 422, "title is required")
		return
	}
	bodyText := strField(body, "body")
	var labelNames []string
	if lv, ok := body["labels"]; ok {
		if arr, ok := lv.([]any); ok {
			for _, item := range arr {
				switch v := item.(type) {
				case string:
					labelNames = append(labelNames, v)
				case map[string]any:
					if n, ok := v["name"].(string); ok {
						labelNames = append(labelNames, n)
					}
				}
			}
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, n := range labelNames {
		if repo.Labels == nil || repo.Labels[n] == nil {
			writeErr(w, 422, "Label does not exist")
			return
		}
	}
	_, ts := store.tickEpoch()
	iss := &Issue{
		Number: repo.NextIssueNum, ID: store.nextIssue,
		Title: title, Body: bodyText, State: "open",
		Labels: labelNames, Created: ts, Updated: ts, Seq: repo.NextIssueNum,
	}
	store.nextIssue++
	repo.NextIssueNum++
	repo.Issues = append(repo.Issues, iss)
	repo.IssuesByNum[iss.Number] = iss
	writeJSON(w, 201, issueJSON(iss))
}

func handleListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	sortField := r.URL.Query().Get("sort")
	direction := r.URL.Query().Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := queryInt(r, "per_page", 30)
	page := queryInt(r, "page", 1)
	store.mu.RLock()
	defer store.mu.RUnlock()
	var list []*Issue
	for _, iss := range repo.Issues {
		if state == "all" || iss.State == state {
			list = append(list, iss)
		}
	}
	_ = sortField // only "created" is used; creation order == number order
	sort.SliceStable(list, func(a, b int) bool {
		if direction == "asc" {
			return list[a].Number < list[b].Number
		}
		return list[a].Number > list[b].Number
	})
	total := len(list)
	start := (page - 1) * perPage
	if start >= total {
		writeJSON(w, 200, []any{})
		return
	}
	end := start + perPage
	if end > total {
		end = total
	}
	out := make([]any, 0, end-start)
	for _, iss := range list[start:end] {
		out = append(out, issueJSON(iss))
	}
	if end < total {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		next := "http://" + r.Host + r.URL.Path + "?" + q.Encode()
		w.Header().Set("Link", "<"+next+">; rel=\"next\"")
	}
	writeJSON(w, 200, out)
}

func handleGetIssue(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	iss := repo.IssuesByNum[n]
	if iss == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, issueJSON(iss))
}

func handlePatchIssue(w http.ResponseWriter, r *http.Request, repo *Repo, n int, body map[string]any) {
	store.mu.Lock()
	defer store.mu.Unlock()
	iss := repo.IssuesByNum[n]
	if iss == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if s, ok := body["state"].(string); ok && (s == "open" || s == "closed") {
		iss.State = s
	}
	if t, ok := body["title"].(string); ok {
		iss.Title = t
	}
	if b, ok := body["body"].(string); ok {
		iss.Body = b
	}
	_, ts := store.tickEpoch()
	iss.Updated = ts
	writeJSON(w, 200, issueJSON(iss))
}

func handleCreateComment(w http.ResponseWriter, r *http.Request, repo *Repo, n int, body map[string]any) {
	text := strField(body, "body")
	if text == "" {
		writeErr(w, 422, "body is required")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	iss := repo.IssuesByNum[n]
	if iss == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	_, ts := store.tickEpoch()
	c := &Comment{ID: store.nextCmt, Body: text, Created: ts}
	store.nextCmt++
	iss.Comments = append(iss.Comments, c)
	writeJSON(w, 201, map[string]any{
		"id": c.ID, "body": c.Body,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID},
		"created_at": ts, "updated_at": ts,
	})
}

func handleListComments(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	iss := repo.IssuesByNum[n]
	if iss == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	out := make([]any, 0, len(iss.Comments))
	for _, c := range iss.Comments {
		out = append(out, map[string]any{
			"id": c.ID, "body": c.Body,
			"user":       map[string]any{"login": viewerLogin, "id": viewerID},
			"created_at": c.Created,
		})
	}
	writeJSON(w, 200, out)
}

// --- pulls -------------------------------------------------------------------------

func pullJSON(p *Pull) map[string]any {
	var mergedAt any
	if p.MergedAt != "" {
		mergedAt = p.MergedAt
	}
	var mergeSha any
	if p.MergeCommitSha != "" {
		mergeSha = p.MergeCommitSha
	}
	return map[string]any{
		"id": p.ID, "number": p.Number, "title": p.Title, "body": p.Body,
		"state": p.State, "merged": p.Merged, "merged_at": mergedAt,
		"merge_commit_sha": mergeSha,
		"head":             map[string]any{"ref": p.HeadRef, "sha": p.HeadSha},
		"base":             map[string]any{"ref": p.BaseRef, "sha": p.BaseSha},
		"user":             map[string]any{"login": viewerLogin, "id": viewerID},
		"created_at":       p.Created,
	}
}

func handleCreatePull(w http.ResponseWriter, r *http.Request, repo *Repo, body map[string]any) {
	title := strField(body, "title")
	head := strField(body, "head")
	base := strField(body, "base")
	if title == "" || head == "" || base == "" {
		writeErr(w, 422, "title, head and base are required")
		return
	}
	bodyText := strField(body, "body")
	store.mu.Lock()
	defer store.mu.Unlock()
	headSha, okH := repo.Refs["refs/heads/"+head]
	baseSha, okB := repo.Refs["refs/heads/"+base]
	if !okH || !okB {
		writeErr(w, 422, "Head or base branch does not exist")
		return
	}
	for _, p := range repo.Pulls {
		if p.State == "open" && p.HeadRef == head {
			writeErr(w, 422, "A pull request already exists for this head")
			return
		}
	}
	_, ts := store.tickEpoch()
	pr := &Pull{
		Number: repo.NextPullNum, ID: store.nextPull,
		Title: title, Body: bodyText, State: "open",
		HeadRef: head, BaseRef: base, HeadSha: headSha, BaseSha: baseSha,
		Created: ts, Seq: repo.NextPullNum,
	}
	store.nextPull++
	repo.NextPullNum++
	repo.Pulls = append(repo.Pulls, pr)
	repo.PullsByNum[pr.Number] = pr
	writeJSON(w, 201, pullJSON(pr))
}

func handleListPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := []any{}
	for _, p := range repo.Pulls {
		if state == "all" || p.State == state {
			out = append(out, pullJSON(p))
		}
	}
	writeJSON(w, 200, out)
}

func handleGetPull(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	pr := repo.PullsByNum[n]
	if pr == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, pullJSON(pr))
}

// mergeBase finds a common ancestor of a and b (first hit BFS from b).
func mergeBase(repo *Repo, a, b string) string {
	ancA := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || ancA[cur] {
			continue
		}
		ancA[cur] = true
		if o := repo.Objects[cur]; o != nil && o.Type == "commit" {
			queue = append(queue, parseCommit(o.Data).Parents...)
		}
	}
	seen := map[string]bool{}
	queue = []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if ancA[cur] {
			return cur
		}
		if o := repo.Objects[cur]; o != nil && o.Type == "commit" {
			queue = append(queue, parseCommit(o.Data).Parents...)
		}
	}
	return ""
}

// flatAt returns path -> flatEntry for a commit's tree.
func (r *Repo) flatAt(commitOID string) map[string]flatEntry {
	out := map[string]flatEntry{}
	tree := r.treeAt(commitOID)
	if tree != "" {
		r.flattenTree(tree, "", out)
	}
	return out
}

// diffFiles computes the merge-base -> head file diff.
func diffFiles(repo *Repo, mb, head string) []map[string]any {
	mbFlat := repo.flatAt(mb)
	headFlat := repo.flatAt(head)
	var paths []string
	for p := range headFlat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := []map[string]any{}
	for _, p := range paths {
		hf := headFlat[p]
		if hf.Dir {
			continue
		}
		mf, inMb := mbFlat[p]
		if inMb && mf.OID == hf.OID {
			continue
		}
		status := "added"
		additions := 0
		deletions := 0
		newData := ""
		if b := repo.Objects[hf.OID]; b != nil {
			newData = string(b.Data)
		}
		newLines := countLines(newData)
		if inMb {
			status = "modified"
			oldData := ""
			if b := repo.Objects[mf.OID]; b != nil {
				oldData = string(b.Data)
			}
			oldLines := countLines(oldData)
			additions = newLines - oldLines
			if additions < 0 {
				deletions = -additions
				additions = 0
			}
			if additions == 0 && deletions == 0 {
				additions = 1
			}
		} else {
			additions = newLines
			if additions == 0 {
				additions = 1
			}
		}
		out = append(out, map[string]any{
			"filename": p, "status": status,
			"additions": additions, "deletions": deletions,
			"changes": additions + deletions,
		})
	}
	return out
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func handlePullFiles(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	pr := repo.PullsByNum[n]
	if pr == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	headTip := pr.HeadSha
	if t, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
		headTip = t
	}
	baseTip := pr.BaseSha
	if t, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
		baseTip = t
	}
	mb := mergeBase(repo, headTip, baseTip)
	files := diffFiles(repo, mb, headTip)
	out := make([]any, 0, len(files))
	for _, f := range files {
		out = append(out, f)
	}
	writeJSON(w, 200, out)
}

// pullConflict reports whether head and base both changed the same file
// differently vs the merge base. Branch tips are resolved live: the base
// branch may have moved since the PR was created.
func pullConflict(repo *Repo, pr *Pull) bool {
	headTip := pr.HeadSha
	if t, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
		headTip = t
	}
	baseTip := pr.BaseSha
	if t, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
		baseTip = t
	}
	mb := mergeBase(repo, headTip, baseTip)
	mbFlat := repo.flatAt(mb)
	baseFlat := repo.flatAt(baseTip)
	headFlat := repo.flatAt(headTip)
	paths := map[string]bool{}
	for p := range mbFlat {
		paths[p] = true
	}
	for p := range baseFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	for p := range paths {
		mbE, inMb := mbFlat[p]
		bE, inB := baseFlat[p]
		hE, inH := headFlat[p]
		if bE.Dir || hE.Dir {
			continue
		}
		bChanged := !inB != !inMb || (inB && inMb && bE.OID != mbE.OID)
		hChanged := !inH != !inMb || (inH && inMb && hE.OID != mbE.OID)
		if bChanged && hChanged {
			bOID := ""
			hOID := ""
			if inB {
				bOID = bE.OID
			}
			if inH {
				hOID = hE.OID
			}
			if bOID != hOID {
				return true
			}
		}
	}
	return false
}

func handleMergePull(w http.ResponseWriter, r *http.Request, repo *Repo, n int, body map[string]any) {
	method := strField(body, "merge_method")
	if method == "" {
		method = "squash"
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	pr := repo.PullsByNum[n]
	if pr == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if pr.State != "open" {
		writeErr(w, 405, "Pull request is already merged or closed")
		return
	}
	if pullConflict(repo, pr) {
		writeErr(w, 405, "Merge conflict")
		return
	}
	// Squash: one commit on the base branch with the head tip's tree.
	headTree := repo.treeAt(pr.HeadSha)
	msg := pr.Title + " (#" + strconv.Itoa(pr.Number) + ")"
	mergeSha := store.commitOnBranch(repo, pr.BaseRef, msg, headTree)
	_, ts := store.tickEpoch()
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = ts
	pr.MergeCommitSha = mergeSha
	pr.BaseSha = mergeSha
	writeJSON(w, 200, map[string]any{
		"sha": mergeSha, "merged": true, "message": "Pull request successfully merged",
	})
}
