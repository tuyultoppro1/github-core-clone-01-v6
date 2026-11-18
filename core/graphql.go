package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// --- mini GraphQL parser -----------------------------------------------------

type gqlSel struct {
	kind     string // "field", "spread", "inline"
	alias    string
	name     string
	args     map[string]any
	sels     []gqlSel
	fragName string
	onType   string
}

type gqlOp struct {
	kind string // "query" | "mutation"
	name string
	sels []gqlSel
}

type gqlFrag struct {
	name   string
	onType string
	sels   []gqlSel
}

type gqlDoc struct {
	ops   []*gqlOp
	frags map[string]*gqlFrag
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

type gqlTok struct {
	kind string // "name","string","int","punct","var","eof"
	val  string
}

func gqlTokenize(src string) []gqlTok {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@':
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{"punct", "..."})
			i += 3
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			toks = append(toks, gqlTok{"var", src[i+1 : j]})
			i = j
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(src[j])
					}
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			toks = append(toks, gqlTok{"string", sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			toks = append(toks, gqlTok{"int", src[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		default:
			i++
		}
	}
	toks = append(toks, gqlTok{"eof", ""})
	return toks
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func (p *gqlParser) peek() gqlTok { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(v string) {
	t := p.next()
	if t.val != v {
		// tolerate: parser is lenient
		_ = t
	}
}

func parseGQL(src string) (*gqlDoc, error) {
	p := &gqlParser{toks: gqlTokenize(src)}
	doc := &gqlDoc{frags: map[string]*gqlFrag{}}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "name" && t.val == "fragment" {
			p.next()
			name := p.next().val
			p.expect("on")
			onType := p.next().val
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.frags[name] = &gqlFrag{name, onType, sels}
			continue
		}
		op := &gqlOp{kind: "query"}
		if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
			op.kind = p.next().val
			if p.peek().kind == "name" {
				op.name = p.next().val
			}
		}
		// variable definitions
		if p.peek().val == "(" {
			p.next()
			for p.peek().val != ")" && p.peek().kind != "eof" {
				p.next()
			}
			p.expect(")")
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		op.sels = sels
		doc.ops = append(doc.ops, op)
	}
	return doc, nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSel, error) {
	p.expect("{")
	var sels []gqlSel
	for p.peek().val != "}" && p.peek().kind != "eof" {
		s, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, s)
	}
	p.expect("}")
	return sels, nil
}

func (p *gqlParser) parseSelection() (gqlSel, error) {
	t := p.peek()
	if t.val == "..." {
		p.next()
		nt := p.peek()
		if nt.kind == "name" && nt.val == "on" {
			p.next()
			onType := p.next().val
			sels, err := p.parseSelectionSet()
			if err != nil {
				return gqlSel{}, err
			}
			return gqlSel{kind: "inline", onType: onType, sels: sels}, nil
		}
		if nt.kind == "name" {
			name := p.next().val
			// skip directives
			for p.peek().val == "@" {
				p.next()
				p.next()
				if p.peek().val == "(" {
					p.skipParens()
				}
			}
			return gqlSel{kind: "spread", fragName: name}, nil
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return gqlSel{}, err
		}
		return gqlSel{kind: "inline", sels: sels}, nil
	}
	// field: [alias:] name [args] [directives] [selectionSet]
	name := p.next().val
	alias := ""
	if p.peek().val == ":" {
		p.next()
		alias = name
		name = p.next().val
	}
	sel := gqlSel{kind: "field", name: name, alias: alias}
	if p.peek().val == "(" {
		args, err := p.parseArgs()
		if err != nil {
			return gqlSel{}, err
		}
		sel.args = args
	}
	for p.peek().val == "@" {
		p.next()
		p.next()
		if p.peek().val == "(" {
			p.skipParens()
		}
	}
	if p.peek().val == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return gqlSel{}, err
		}
		sel.sels = sels
	}
	return sel, nil
}

func (p *gqlParser) skipParens() {
	depth := 0
	for p.peek().kind != "eof" {
		t := p.next()
		if t.val == "(" || t.val == "[" || t.val == "{" {
			depth++
		} else if t.val == ")" || t.val == "]" || t.val == "}" {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *gqlParser) parseArgs() (map[string]any, error) {
	args := map[string]any{}
	p.expect("(")
	for p.peek().val != ")" && p.peek().kind != "eof" {
		name := p.next().val
		p.expect(":")
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
	p.expect(")")
	return args, nil
}

type gqlVar struct{ name string }

func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	switch t.kind {
	case "var":
		return gqlVar{t.val}, nil
	case "string":
		return t.val, nil
	case "int":
		if n, err := strconv.Atoi(t.val); err == nil {
			return n, nil
		}
		f, _ := strconv.ParseFloat(t.val, 64)
		return f, nil
	case "name":
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return t.val, nil // enum
		}
	case "punct":
		if t.val == "[" {
			var arr []any
			for p.peek().val != "]" && p.peek().kind != "eof" {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			p.expect("]")
			return arr, nil
		}
		if t.val == "{" {
			obj := map[string]any{}
			for p.peek().val != "}" && p.peek().kind != "eof" {
				name := p.next().val
				p.expect(":")
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[name] = v
			}
			p.expect("}")
			return obj, nil
		}
	}
	return nil, fmt.Errorf("unexpected token %v", t)
}

// --- execution ---------------------------------------------------------------

type gqlObj struct {
	kind string
	repo *Repo
	iss  *Issue
	pull *Pull
	// connections
	issues    []*Issue
	pulls     []*Pull
	files     []map[string]any
	hasNext   bool
	endCursor string
	// introspection
	typeName string
	// payloads
	payloadIssue   *Issue
	payloadPull    *Pull
	payloadComment *Comment
	commentEdge    *Comment
}

func resolveVars(v any, vars map[string]any) any {
	switch t := v.(type) {
	case gqlVar:
		return vars[t.name]
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			out[k] = resolveVars(x, vars)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = resolveVars(x, vars)
		}
		return out
	default:
		return v
	}
}

func argStr(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		}
	}
	return def
}

func argStrList(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// execSel executes a selection set against an object, returning its JSON map.
func execSel(doc *gqlDoc, obj *gqlObj, sels []gqlSel, vars map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, sel := range sels {
		switch sel.kind {
		case "spread":
			frag, ok := doc.frags[sel.fragName]
			if !ok {
				return nil, fmt.Errorf("unknown fragment %q", sel.fragName)
			}
			if frag.onType != "" && frag.onType != obj.kind {
				continue
			}
			sub, err := execSel(doc, obj, frag.sels, vars)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
		case "inline":
			if sel.onType != "" && sel.onType != obj.kind {
				continue
			}
			sub, err := execSel(doc, obj, sel.sels, vars)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
		case "field":
			key := sel.name
			if sel.alias != "" {
				key = sel.alias
			}
			args := map[string]any{}
			for k, v := range sel.args {
				args[k] = resolveVars(v, vars)
			}
			val, err := resolveField(doc, obj, sel, args, vars)
			if err != nil {
				return nil, err
			}
			out[key] = val
		}
	}
	return out, nil
}

// resolveField resolves one field of an object.
func resolveField(doc *gqlDoc, obj *gqlObj, sel gqlSel, args map[string]any, vars map[string]any) (any, error) {
	name := sel.name
	sub := func(child *gqlObj) (any, error) {
		if len(sel.sels) == 0 {
			return nil, fmt.Errorf("field %q requires a selection set", name)
		}
		return execSel(doc, child, sel.sels, vars)
	}
	subList := func(children []*gqlObj) (any, error) {
		out := make([]any, 0, len(children))
		for _, c := range children {
			v, err := sub(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	unknown := func() (any, error) {
		return nil, fmt.Errorf("Cannot query field %q on type %q.", name, obj.kind)
	}

	switch obj.kind {
	case "Query":
		switch name {
		case "viewer":
			return sub(&gqlObj{kind: "User"})
		case "repository":
			owner := argStr(args, "owner")
			rname := argStr(args, "name")
			store.mu.RLock()
			repo := store.findRepo(owner, rname)
			store.mu.RUnlock()
			if repo == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "Repository", repo: repo})
		case "node":
			id := argStr(args, "id")
			kind, n, ok := parseGlobalID(id)
			if !ok {
				return nil, nil
			}
			store.mu.RLock()
			defer store.mu.RUnlock()
			switch kind {
			case "R":
				for _, rp := range store.repoSeq {
					if rp.DatabaseID == n {
						return sub(&gqlObj{kind: "Repository", repo: rp})
					}
				}
			case "I":
				for _, rp := range store.repoSeq {
					for _, iss := range rp.Issues {
						if iss.ID == n {
							return sub(&gqlObj{kind: "Issue", repo: rp, iss: iss})
						}
					}
				}
			case "P":
				for _, rp := range store.repoSeq {
					for _, pr := range rp.Pulls {
						if pr.ID == n {
							return sub(&gqlObj{kind: "PullRequest", repo: rp, pull: pr})
						}
					}
				}
			}
			return nil, nil
		case "__type":
			tname := argStr(args, "name")
			if _, ok := gqlTypeFields[tname]; ok {
				return sub(&gqlObj{kind: "__Type", typeName: tname})
			}
			if tname == "SearchType" {
				return sub(&gqlObj{kind: "__Type", typeName: tname})
			}
			return nil, nil
		}
		return unknown()
	case "Mutation":
		return resolveMutation(doc, obj, sel, args, vars)
	case "User":
		switch name {
		case "login":
			return viewerLogin, nil
		case "name":
			return viewerName, nil
		case "id":
			return idFor("U", viewerID), nil
		}
		return unknown()
	case "Repository":
		r := obj.repo
		switch name {
		case "id":
			return idFor("R", r.DatabaseID), nil
		case "databaseId":
			return r.DatabaseID, nil
		case "name":
			return r.Name, nil
		case "nameWithOwner":
			return r.Owner + "/" + r.Name, nil
		case "owner":
			return sub(&gqlObj{kind: "User"})
		case "defaultBranchRef":
			store.mu.RLock()
			tip, ok := r.Refs["refs/heads/"+r.Default]
			store.mu.RUnlock()
			if !ok {
				_ = tip
				return nil, nil
			}
			return sub(&gqlObj{kind: "Ref", typeName: r.Default})
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "description":
			return r.Description, nil
		case "private":
			return r.Private, nil
		case "issue":
			num := argInt(args, "number", 0)
			store.mu.RLock()
			iss := r.IssuesByNum[num]
			store.mu.RUnlock()
			if iss == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "Issue", repo: r, iss: iss})
		case "issues":
			return sub(issuesConnection(r, args))
		case "pullRequests":
			return sub(pullsConnection(r, args))
		case "pullRequest":
			num := argInt(args, "number", 0)
			store.mu.RLock()
			pr := r.PullsByNum[num]
			store.mu.RUnlock()
			if pr == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "PullRequest", repo: r, pull: pr})
		}
		return unknown()
	case "Issue":
		iss := obj.iss
		switch name {
		case "id":
			return idFor("I", iss.ID), nil
		case "number":
			return iss.Number, nil
		case "title":
			return iss.Title, nil
		case "state":
			return strings.ToUpper(iss.State), nil
		case "body":
			return iss.Body, nil
		case "url":
			return "https://github.com/" + obj.repo.Owner + "/" + obj.repo.Name + "/issues/" + strconv.Itoa(iss.Number), nil
		case "createdAt":
			return iss.Created, nil
		}
		return unknown()
	case "IssueConnection":
		switch name {
		case "nodes":
			children := make([]*gqlObj, 0, len(obj.issues))
			for _, iss := range obj.issues {
				children = append(children, &gqlObj{kind: "Issue", repo: obj.repo, iss: iss})
			}
			return subList(children)
		case "pageInfo":
			return sub(&gqlObj{kind: "PageInfo", hasNext: obj.hasNext, endCursor: obj.endCursor})
		case "totalCount":
			return len(obj.issues), nil
		}
		return unknown()
	case "PullRequestConnection":
		switch name {
		case "nodes":
			children := make([]*gqlObj, 0, len(obj.pulls))
			for _, pr := range obj.pulls {
				children = append(children, &gqlObj{kind: "PullRequest", repo: obj.repo, pull: pr})
			}
			return subList(children)
		case "pageInfo":
			return sub(&gqlObj{kind: "PageInfo", hasNext: obj.hasNext, endCursor: obj.endCursor})
		case "totalCount":
			return len(obj.pulls), nil
		}
		return unknown()
	case "PullRequest":
		pr := obj.pull
		switch name {
		case "id":
			return idFor("P", pr.ID), nil
		case "number":
			return pr.Number, nil
		case "title":
			return pr.Title, nil
		case "body":
			return pr.Body, nil
		case "state":
			if pr.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pr.State), nil
		case "merged":
			return pr.Merged, nil
		case "headRefName":
			return pr.HeadRef, nil
		case "baseRefName":
			return pr.BaseRef, nil
		case "mergeable":
			store.mu.RLock()
			conflict := pullConflict(obj.repo, pr)
			store.mu.RUnlock()
			if conflict {
				return "CONFLICTING", nil
			}
			return "MERGEABLE", nil
		case "files":
			store.mu.RLock()
			headTip := pr.HeadSha
			if t, ok := obj.repo.Refs["refs/heads/"+pr.HeadRef]; ok {
				headTip = t
			}
			baseTip := pr.BaseSha
			if t, ok := obj.repo.Refs["refs/heads/"+pr.BaseRef]; ok {
				baseTip = t
			}
			mb := mergeBase(obj.repo, headTip, baseTip)
			files := diffFiles(obj.repo, mb, headTip)
			store.mu.RUnlock()
			return sub(&gqlObj{kind: "ChangedFileConnection", files: files})
		}
		return unknown()
	case "ChangedFileConnection":
		switch name {
		case "nodes":
			children := make([]*gqlObj, 0, len(obj.files))
			for _, f := range obj.files {
				children = append(children, &gqlObj{kind: "ChangedFile", typeName: f["filename"].(string)})
			}
			return subList(children)
		}
		return unknown()
	case "ChangedFile":
		if name == "path" {
			return obj.typeName, nil
		}
		return unknown()
	case "PageInfo":
		switch name {
		case "hasNextPage":
			return obj.hasNext, nil
		case "endCursor":
			if obj.endCursor == "" {
				return nil, nil
			}
			return obj.endCursor, nil
		}
		return unknown()
	case "Ref":
		if name == "name" {
			return obj.typeName, nil
		}
		return unknown()
	case "__Type":
		switch name {
		case "name":
			return obj.typeName, nil
		case "kind":
			if obj.typeName == "SearchType" {
				return "ENUM", nil
			}
			return "OBJECT", nil
		case "fields":
			fields := gqlTypeFields[obj.typeName]
			children := make([]*gqlObj, 0, len(fields))
			for _, f := range fields {
				children = append(children, &gqlObj{kind: "__Field", typeName: f})
			}
			return subList(children)
		case "enumValues":
			if obj.typeName != "SearchType" {
				return nil, nil
			}
			children := []*gqlObj{}
			for _, e := range []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE"} {
				children = append(children, &gqlObj{kind: "__EnumValue", typeName: e})
			}
			return subList(children)
		}
		return unknown()
	case "__Field":
		if name == "name" {
			return obj.typeName, nil
		}
		return unknown()
	case "__EnumValue":
		if name == "name" {
			return obj.typeName, nil
		}
		return unknown()
	case "CreateIssuePayload":
		if name == "issue" {
			if obj.payloadIssue == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "Issue", repo: obj.repo, iss: obj.payloadIssue})
		}
		return unknown()
	case "CloseIssuePayload":
		if name == "issue" {
			if obj.payloadIssue == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "Issue", repo: obj.repo, iss: obj.payloadIssue})
		}
		return unknown()
	case "CreatePullRequestPayload":
		if name == "pullRequest" {
			if obj.payloadPull == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "PullRequest", repo: obj.repo, pull: obj.payloadPull})
		}
		return unknown()
	case "MergePullRequestPayload":
		if name == "pullRequest" {
			if obj.payloadPull == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "PullRequest", repo: obj.repo, pull: obj.payloadPull})
		}
		return unknown()
	case "AddCommentPayload":
		if name == "commentEdge" {
			if obj.payloadComment == nil {
				return nil, nil
			}
			return sub(&gqlObj{kind: "CommentEdge", commentEdge: obj.payloadComment})
		}
		return unknown()
	case "CommentEdge":
		if name == "node" {
			return sub(&gqlObj{kind: "Comment", commentEdge: obj.commentEdge})
		}
		return unknown()
	case "Comment":
		switch name {
		case "id":
			return idFor("C", obj.commentEdge.ID), nil
		case "body":
			return obj.commentEdge.Body, nil
		}
		return unknown()
	}
	return unknown()
}

// --- introspection field tables ----------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "url", "createdAt", "author", "comments", "labels"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body", "url", "createdAt", "author", "headRefOid", "baseRefOid"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt"},
}

// --- connections ---------------------------------------------------------------

func issuesConnection(r *Repo, args map[string]any) *gqlObj {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var list []*Issue
	states := argStrList(args, "states")
	for _, iss := range r.Issues {
		if len(states) > 0 {
			match := false
			for _, s := range states {
				if strings.EqualFold(s, iss.State) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		list = append(list, iss)
	}
	// orderBy
	desc := false
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if d, ok := ob["direction"].(string); ok && strings.EqualFold(d, "DESC") {
			desc = true
		}
	}
	sort.SliceStable(list, func(a, b int) bool {
		if desc {
			return list[a].Number > list[b].Number
		}
		return list[a].Number < list[b].Number
	})
	first := argInt(args, "first", 0)
	after := argStr(args, "after")
	start := 0
	if after != "" {
		if b, err := base64.RawURLEncoding.DecodeString(after); err == nil {
			var n int
			if _, err := fmt.Sscanf(string(b), "cursor:%d", &n); err == nil {
				// cursor is the last issue number on the previous page
				for i, iss := range list {
					if iss.Number == n {
						start = i + 1
						break
					}
				}
			}
		}
	}
	conn := &gqlObj{kind: "IssueConnection", repo: r}
	if first <= 0 {
		first = len(list)
	}
	end := start + first
	hasNext := false
	if end < len(list) {
		hasNext = true
	} else {
		end = len(list)
	}
	if start > len(list) {
		start = len(list)
	}
	conn.issues = list[start:end]
	conn.hasNext = hasNext
	if len(conn.issues) > 0 {
		last := conn.issues[len(conn.issues)-1].Number
		conn.endCursor = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", last)))
	}
	return conn
}

func pullsConnection(r *Repo, args map[string]any) *gqlObj {
	store.mu.RLock()
	defer store.mu.RUnlock()
	first := argInt(args, "first", 0)
	list := r.Pulls
	if first > 0 && first < len(list) {
		list = list[:first]
	}
	return &gqlObj{kind: "PullRequestConnection", repo: r, pulls: list}
}

// --- mutations ------------------------------------------------------------------

func resolveMutation(doc *gqlDoc, obj *gqlObj, sel gqlSel, args map[string]any, vars map[string]any) (any, error) {
	name := sel.name
	sub := func(child *gqlObj) (any, error) {
		return execSel(doc, child, sel.sels, vars)
	}
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = args
	}
	switch name {
	case "createIssue":
		repoID := ""
		if v, ok := input["repositoryId"].(string); ok {
			repoID = v
		}
		kind, n, ok := parseGlobalID(repoID)
		if !ok || kind != "R" {
			return nil, fmt.Errorf("repository not found")
		}
		title, _ := input["title"].(string)
		bodyText, _ := input["body"].(string)
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repoSeq {
			if rp.DatabaseID == n {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return nil, fmt.Errorf("repository not found")
		}
		_, ts := store.tickEpoch()
		iss := &Issue{
			Number: repo.NextIssueNum, ID: store.nextIssue,
			Title: title, Body: bodyText, State: "open", Created: ts, Updated: ts,
		}
		store.nextIssue++
		repo.NextIssueNum++
		repo.Issues = append(repo.Issues, iss)
		repo.IssuesByNum[iss.Number] = iss
		store.mu.Unlock()
		return sub(&gqlObj{kind: "CreateIssuePayload", repo: repo, payloadIssue: iss})
	case "closeIssue":
		issueID, _ := input["issueId"].(string)
		kind, n, ok := parseGlobalID(issueID)
		if !ok || kind != "I" {
			return nil, fmt.Errorf("issue not found")
		}
		store.mu.Lock()
		var found *Issue
		var foundRepo *Repo
		for _, rp := range store.repoSeq {
			for _, iss := range rp.Issues {
				if iss.ID == n {
					found = iss
					foundRepo = rp
				}
			}
		}
		if found == nil {
			store.mu.Unlock()
			return nil, fmt.Errorf("issue not found")
		}
		found.State = "closed"
		_, ts := store.tickEpoch()
		found.Updated = ts
		store.mu.Unlock()
		return sub(&gqlObj{kind: "CloseIssuePayload", repo: foundRepo, payloadIssue: found})
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		kind, n, ok := parseGlobalID(repoID)
		if !ok || kind != "R" {
			return nil, fmt.Errorf("repository not found")
		}
		baseRef, _ := input["baseRefName"].(string)
		headRef, _ := input["headRefName"].(string)
		title, _ := input["title"].(string)
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repoSeq {
			if rp.DatabaseID == n {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return nil, fmt.Errorf("repository not found")
		}
		headSha, okH := repo.Refs["refs/heads/"+headRef]
		baseSha, okB := repo.Refs["refs/heads/"+baseRef]
		if !okH || !okB {
			store.mu.Unlock()
			return nil, fmt.Errorf("branch not found")
		}
		_, ts := store.tickEpoch()
		pr := &Pull{
			Number: repo.NextPullNum, ID: store.nextPull,
			Title: title, State: "open",
			HeadRef: headRef, BaseRef: baseRef, HeadSha: headSha, BaseSha: baseSha,
			Created: ts,
		}
		store.nextPull++
		repo.NextPullNum++
		repo.Pulls = append(repo.Pulls, pr)
		repo.PullsByNum[pr.Number] = pr
		store.mu.Unlock()
		return sub(&gqlObj{kind: "CreatePullRequestPayload", repo: repo, payloadPull: pr})
	case "mergePullRequest":
		prID, _ := input["pullRequestId"].(string)
		kind, n, ok := parseGlobalID(prID)
		if !ok || kind != "P" {
			return nil, fmt.Errorf("pull request not found")
		}
		store.mu.Lock()
		var pr *Pull
		var repo *Repo
		for _, rp := range store.repoSeq {
			for _, p := range rp.Pulls {
				if p.ID == n {
					pr = p
					repo = rp
				}
			}
		}
		if pr == nil {
			store.mu.Unlock()
			return nil, fmt.Errorf("pull request not found")
		}
		if pr.State != "open" || pullConflict(repo, pr) {
			store.mu.Unlock()
			return nil, fmt.Errorf("pull request cannot be merged")
		}
		headTree := repo.treeAt(pr.HeadSha)
		msg := pr.Title + " (#" + strconv.Itoa(pr.Number) + ")"
		mergeSha := store.commitOnBranch(repo, pr.BaseRef, msg, headTree)
		_, ts := store.tickEpoch()
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = ts
		pr.MergeCommitSha = mergeSha
		pr.BaseSha = mergeSha
		store.mu.Unlock()
		return sub(&gqlObj{kind: "MergePullRequestPayload", repo: repo, payloadPull: pr})
	case "addComment":
		subjectID, _ := input["subjectId"].(string)
		bodyText, _ := input["body"].(string)
		kind, n, ok := parseGlobalID(subjectID)
		if !ok || kind != "I" {
			return nil, fmt.Errorf("subject not found")
		}
		store.mu.Lock()
		var found *Issue
		var foundRepo *Repo
		for _, rp := range store.repoSeq {
			for _, iss := range rp.Issues {
				if iss.ID == n {
					found = iss
					foundRepo = rp
				}
			}
		}
		if found == nil {
			store.mu.Unlock()
			return nil, fmt.Errorf("subject not found")
		}
		_, ts := store.tickEpoch()
		c := &Comment{ID: store.nextCmt, Body: bodyText, Created: ts}
		store.nextCmt++
		found.Comments = append(found.Comments, c)
		store.mu.Unlock()
		return sub(&gqlObj{kind: "AddCommentPayload", repo: foundRepo, payloadComment: c})
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", name, obj.kind)
}

// --- HTTP handler ---------------------------------------------------------------

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	raw, err := readAllBody(r)
	if err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	// Batch: JSON array of operations.
	var batch []json.RawMessage
	isBatch := false
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &batch); err == nil {
			isBatch = true
		}
	}
	var ops []json.RawMessage
	if isBatch {
		ops = batch
	} else {
		ops = []json.RawMessage{raw}
	}
	results := make([]any, 0, len(ops))
	for _, opRaw := range ops {
		results = append(results, execGraphQLOp(opRaw))
	}
	if isBatch {
		writeJSON(w, 200, results)
	} else {
		writeJSON(w, 200, results[0])
	}
}

func execGraphQLOp(opRaw json.RawMessage) any {
	var req struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.Unmarshal(opRaw, &req); err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": "invalid request"}}}
	}
	if req.Variables == nil {
		req.Variables = map[string]any{}
	}
	doc, err := parseGQL(req.Query)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	var op *gqlOp
	if req.OperationName != "" {
		for _, o := range doc.ops {
			if o.name == req.OperationName {
				op = o
				break
			}
		}
	}
	if op == nil && len(doc.ops) > 0 {
		op = doc.ops[0]
	}
	if op == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "no operation"}}}
	}
	rootKind := "Query"
	if op.kind == "mutation" {
		rootKind = "Mutation"
	}
	data, err := execSel(doc, &gqlObj{kind: rootKind}, op.sels, req.Variables)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []any{map[string]any{"message": err.Error()}},
		}
	}
	return map[string]any{"data": data}
}
