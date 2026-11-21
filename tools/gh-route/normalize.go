package main

import "strings"

type User struct {
	Login string `json:"login"`
}

type ReviewOut struct {
	User        User    `json:"user"`
	State       string  `json:"state"`
	SubmittedAt string  `json:"submitted_at"`
	CommitID    *string `json:"commit_id"`
	Body        string  `json:"body"`
}

type CommentOut struct {
	User      User   `json:"user"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ReviewCommentOut struct {
	User     User    `json:"user"`
	Body     string  `json:"body"`
	Path     string  `json:"path"`
	Line     *int64  `json:"line"`
	CommitID *string `json:"commit_id"`
}

type CheckRun struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

type CheckRunsOut struct {
	CheckRuns []CheckRun `json:"check_runs"`
}

type PullRequestOut struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	User User `json:"user"`
}

func stringPointer(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

func intPointer(value any) *int64 {
	switch number := value.(type) {
	case float64:
		converted := int64(number)
		return &converted
	case int64:
		return &number
	case int:
		converted := int64(number)
		return &converted
	default:
		return nil
	}
}

func records(value any) []map[string]any {
	items := asArray(value)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, asRecord(item))
	}
	return out
}

func connectionNodes(parent map[string]any, key string) []map[string]any {
	return records(nested(parent[key], "nodes"))
}

func graphqlPullRequest(response any) map[string]any {
	return objectAt(response, "data", "repository", "pullRequest")
}

func reviewsFromGraphQL(response any) []ReviewOut {
	nodes := connectionNodes(graphqlPullRequest(response), "reviews")
	out := make([]ReviewOut, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, ReviewOut{
			User:        User{Login: gqlLogin(node["author"])},
			State:       asString(node["state"]),
			SubmittedAt: asString(node["submittedAt"]),
			CommitID:    stringPointer(objectAt(node["commit"])["oid"]),
			Body:        asString(node["body"]),
		})
	}
	return out
}

func reviewsFromREST(pages []any) []ReviewOut {
	items := slurpAdd(pages)
	out := make([]ReviewOut, 0, len(items))
	for _, item := range items {
		out = append(out, ReviewOut{
			User:        User{Login: asString(objectAt(item, "user")["login"])},
			State:       asString(item["state"]),
			SubmittedAt: asString(item["submitted_at"]),
			CommitID:    stringPointer(item["commit_id"]),
			Body:        asString(item["body"]),
		})
	}
	return out
}

func commentsFromGraphQL(response any) []CommentOut {
	nodes := connectionNodes(graphqlPullRequest(response), "comments")
	out := make([]CommentOut, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, CommentOut{
			User:      User{Login: gqlLogin(node["author"])},
			Body:      asString(node["body"]),
			CreatedAt: asString(node["createdAt"]),
			UpdatedAt: asString(node["updatedAt"]),
		})
	}
	return out
}

func commentsFromREST(pages []any) []CommentOut {
	items := slurpAdd(pages)
	out := make([]CommentOut, 0, len(items))
	for _, item := range items {
		out = append(out, CommentOut{
			User:      User{Login: asString(objectAt(item, "user")["login"])},
			Body:      asString(item["body"]),
			CreatedAt: asString(item["created_at"]),
			UpdatedAt: asString(item["updated_at"]),
		})
	}
	return out
}

func reviewCommentNodes(response any) []map[string]any {
	threads := connectionNodes(graphqlPullRequest(response), "reviewThreads")
	count := 0
	for _, thread := range threads {
		count += len(connectionNodes(thread, "comments"))
	}
	out := make([]map[string]any, 0, count)
	for _, thread := range threads {
		out = append(out, connectionNodes(thread, "comments")...)
	}
	return out
}

func reviewCommentsFromGraphQL(response any) []ReviewCommentOut {
	nodes := reviewCommentNodes(response)
	out := make([]ReviewCommentOut, 0, len(nodes))
	for _, node := range nodes {
		commitID := stringPointer(objectAt(node, "commit")["oid"])
		if commitID == nil {
			commitID = stringPointer(objectAt(node, "originalCommit")["oid"])
		}
		out = append(out, ReviewCommentOut{
			User:     User{Login: gqlLogin(node["author"])},
			Body:     asString(node["body"]),
			Path:     asString(node["path"]),
			Line:     intPointer(node["line"]),
			CommitID: commitID,
		})
	}
	return out
}

func reviewCommentsFromREST(pages []any) []ReviewCommentOut {
	items := slurpAdd(pages)
	out := make([]ReviewCommentOut, 0, len(items))
	for _, item := range items {
		out = append(out, ReviewCommentOut{
			User:     User{Login: asString(objectAt(item, "user")["login"])},
			Body:     asString(item["body"]),
			Path:     asString(item["path"]),
			Line:     intPointer(item["line"]),
			CommitID: stringPointer(item["commit_id"]),
		})
	}
	return out
}

func checkRunsFromGraphQL(response any) CheckRunsOut {
	object := objectAt(response, "data", "repository", "object")
	suites := connectionNodes(object, "checkSuites")
	runs := []CheckRun{}
	for _, suite := range suites {
		for _, node := range connectionNodes(suite, "checkRuns") {
			var conclusion *string
			if value, ok := node["conclusion"].(string); ok {
				lower := strings.ToLower(value)
				conclusion = &lower
			}
			runs = append(runs, CheckRun{
				Name:       asString(node["name"]),
				Status:     strings.ToLower(asString(node["status"])),
				Conclusion: conclusion,
			})
		}
	}
	return CheckRunsOut{CheckRuns: runs}
}

func checkRunsFromREST(pages []any) CheckRunsOut {
	runs := []CheckRun{}
	for _, page := range pages {
		for _, node := range records(objectAt(page)["check_runs"]) {
			runs = append(runs, CheckRun{
				Name:       asString(node["name"]),
				Status:     asString(node["status"]),
				Conclusion: stringPointer(node["conclusion"]),
			})
		}
	}
	return CheckRunsOut{CheckRuns: runs}
}

func pullRequestsFromGraphQL(response any) []PullRequestOut {
	repository := objectAt(response, "data", "repository")
	nodes := connectionNodes(repository, "pullRequests")
	out := make([]PullRequestOut, 0, len(nodes))
	for _, node := range nodes {
		var number int64
		if value, ok := node["number"].(float64); ok {
			number = int64(value)
		}
		request := PullRequestOut{
			Number: number,
			Title:  asString(node["title"]),
			State:  strings.ToLower(asString(node["state"])),
			User:   User{Login: gqlLogin(node["author"])},
		}
		request.Head.SHA = asString(node["headRefOid"])
		out = append(out, request)
	}
	return out
}

func pullRequestsFromREST(pages []any) []PullRequestOut {
	items := slurpAdd(pages)
	out := make([]PullRequestOut, 0, len(items))
	for _, item := range items {
		var number int64
		if value, ok := item["number"].(float64); ok {
			number = int64(value)
		}
		request := PullRequestOut{
			Number: number,
			Title:  asString(item["title"]),
			State:  asString(item["state"]),
			User:   User{Login: asString(objectAt(item, "user")["login"])},
		}
		request.Head.SHA = asString(objectAt(item, "head")["sha"])
		out = append(out, request)
	}
	return out
}

func headSHAFromGraphQL(response any) string {
	return asString(objectAt(response, "data", "repository", "pullRequest")["headRefOid"])
}

func headSHAFromREST(response any) string {
	return asString(objectAt(response, "head")["sha"])
}
