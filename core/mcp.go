package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// --- MCP (Streamable HTTP JSON-RPC 2.0, protocol 2026-07-28) --------------------

type mcpToolsDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpTools = []mcpToolsDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository for the viewer",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"private":     map[string]any{"type": "boolean"},
		}, "required": []string{"name"}},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo"}},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
			"title": map[string]any{"type": "string"},
			"body":  map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo", "title"}},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
			"state": map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo"}},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request in a repository",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
			"title": map[string]any{"type": "string"},
			"head":  map[string]any{"type": "string"},
			"base":  map[string]any{"type": "string"},
			"body":  map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request in a repository",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner":       map[string]any{"type": "string"},
			"repo":        map[string]any{"type": "string"},
			"pull_number": map[string]any{"type": "integer"},
		}, "required": []string{"owner", "repo", "pull_number"}},
	},
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		w.WriteHeader(405)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	raw, err := readAllBody(r)
	if err != nil {
		writeJSON(w, 400, mcpError(nil, -32700, "Parse error"))
		return
	}
	// Non-JSON body -> HTTP 400, -32700.
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, 400, mcpError(nil, -32700, "Parse error"))
		return
	}
	method, _ := req["method"].(string)
	id := req["id"]
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	// Notifications (no id) -> empty 200.
	if id == nil {
		w.WriteHeader(200)
		return
	}
	switch method {
	case "initialize":
		writeJSON(w, 200, mcpResult(id, map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gitforge-v6", "version": "1.0.0"},
		}))
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(200)
	case "ping":
		writeJSON(w, 200, mcpResult(id, map[string]any{}))
	case "server/discover":
		writeJSON(w, 200, mcpResult(id, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "gitforge-v6", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "gitforge-v6", "version": "1.0.0"},
			},
		}))
	case "tools/list":
		writeJSON(w, 200, mcpResult(id, map[string]any{"tools": mcpTools}))
	case "tools/call":
		handleMCPToolCall(w, r, id, params)
	default:
		// Unknown method: -32601, never HTTP 500.
		writeJSON(w, 200, mcpError(id, -32601, "Method not found"))
	}
}

func mcpResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpError(id any, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func mcpToolResult(id any, structured any, isError bool) map[string]any {
	text := string(jsonBytes(structured))
	res := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": structured,
	}
	if isError {
		res["isError"] = true
	}
	return mcpResult(id, res)
}

func handleMCPToolCall(w http.ResponseWriter, r *http.Request, id any, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	known := false
	for _, t := range mcpTools {
		if t.Name == name {
			known = true
		}
	}
	if !known {
		writeJSON(w, 200, mcpError(id, -32602, "Unknown tool: "+name))
		return
	}
	// required args
	for _, t := range mcpTools {
		if t.Name != name {
			continue
		}
		if req, ok := t.InputSchema["required"].([]string); ok {
			for _, reqArg := range req {
				if _, ok := args[reqArg]; !ok {
					writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "missing required argument: " + reqArg}, true))
					return
				}
			}
		}
	}
	owner, _ := args["owner"].(string)
	repoName, _ := args["repo"].(string)
	switch name {
	case "create_repo":
		rname, _ := args["name"].(string)
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		store.mu.Lock()
		if store.findRepo(viewerLogin, rname) != nil {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Repository already exists"}, true))
			return
		}
		repo := store.createRepo(rname, desc, priv)
		store.mu.Unlock()
		writeJSON(w, 200, mcpToolResult(id, map[string]any{
			"id": repo.DatabaseID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
			"private": repo.Private, "default_branch": repo.Default,
		}, false))
	case "get_repo":
		store.mu.RLock()
		repo := store.findRepo(owner, repoName)
		store.mu.RUnlock()
		if repo == nil {
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Not Found"}, true))
			return
		}
		writeJSON(w, 200, mcpToolResult(id, map[string]any{
			"id": repo.DatabaseID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
			"private": repo.Private, "default_branch": repo.Default,
			"description": repo.Description,
		}, false))
	case "create_issue":
		title, _ := args["title"].(string)
		bodyText, _ := args["body"].(string)
		store.mu.Lock()
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Not Found"}, true))
			return
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
		writeJSON(w, 200, mcpToolResult(id, map[string]any{
			"id": iss.ID, "number": iss.Number, "title": iss.Title, "body": iss.Body, "state": iss.State,
		}, false))
	case "list_issues":
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		store.mu.RLock()
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			store.mu.RUnlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Not Found"}, true))
			return
		}
		out := []any{}
		for _, iss := range repo.Issues {
			if state == "all" || iss.State == state {
				out = append(out, map[string]any{
					"id": iss.ID, "number": iss.Number, "title": iss.Title, "body": iss.Body, "state": iss.State,
				})
			}
		}
		store.mu.RUnlock()
		writeJSON(w, 200, mcpToolResult(id, map[string]any{"issues": out}, false))
	case "create_pull_request":
		title, _ := args["title"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		bodyText, _ := args["body"].(string)
		store.mu.Lock()
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Not Found"}, true))
			return
		}
		headSha, okH := repo.Refs["refs/heads/"+head]
		baseSha, okB := repo.Refs["refs/heads/"+base]
		if !okH || !okB {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Head or base branch does not exist"}, true))
			return
		}
		for _, p := range repo.Pulls {
			if p.State == "open" && p.HeadRef == head {
				store.mu.Unlock()
				writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "A pull request already exists for this head"}, true))
				return
			}
		}
		_, ts := store.tickEpoch()
		pr := &Pull{
			Number: repo.NextPullNum, ID: store.nextPull,
			Title: title, Body: bodyText, State: "open",
			HeadRef: head, BaseRef: base, HeadSha: headSha, BaseSha: baseSha, Created: ts,
		}
		store.nextPull++
		repo.NextPullNum++
		repo.Pulls = append(repo.Pulls, pr)
		repo.PullsByNum[pr.Number] = pr
		store.mu.Unlock()
		writeJSON(w, 200, mcpToolResult(id, map[string]any{
			"id": pr.ID, "number": pr.Number, "title": pr.Title, "state": pr.State,
			"head": map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSha},
			"base": map[string]any{"ref": pr.BaseRef, "sha": pr.BaseSha},
		}, false))
	case "merge_pull_request":
		var num int
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case json.Number:
			n, _ := v.Int64()
			num = int(n)
		case string:
			num, _ = strconv.Atoi(v)
		case int:
			num = v
		}
		store.mu.Lock()
		repo := store.findRepo(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Not Found"}, true))
			return
		}
		pr := repo.PullsByNum[num]
		if pr == nil {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Not Found"}, true))
			return
		}
		if pr.State != "open" || pullConflict(repo, pr) {
			store.mu.Unlock()
			writeJSON(w, 200, mcpToolResult(id, map[string]any{"error": "Pull request cannot be merged"}, true))
			return
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
		writeJSON(w, 200, mcpToolResult(id, map[string]any{
			"sha": mergeSha, "merged": true, "number": pr.Number,
		}, false))
	}
}

var _ = fmt.Sprintf
var _ = strings.Contains
