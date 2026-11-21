package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec" //nolint:depguard // Resolve the caller's current branch with a fixed git command.
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/credentialfile"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const (
	timeoutSeconds    = 10
	pipelinesPageSize = 50
	pipelinesMaxPages = 5
)

const usage = `usage: ci-queue [--branch <name>] [--pipeline <number>] [--repo <owner/name>]

Lists the Woodpecker queue tasks belonging to a branch's pipelines (or one
explicit pipeline), with each pending task's rank in the dispatch order.

  --branch <name>       branch to look up (default: $WOODPECKER_BRANCH, else the
                        current git branch)
  --pipeline <number>   a single pipeline number; skips the branch lookup
  --repo <owner/name>   repo full name to resolve (required unless WOODPECKER_REPO
                        is set)
  -h, --help            print this help and exit

Environment: WOODPECKER_SERVER and WOODPECKER_TOKEN (the variables woodpecker-cli
already uses). The token is never echoed.
`

type QueueState string

const (
	QueuePending       QueueState = "pending"
	QueueWaitingOnDeps QueueState = "waiting_on_deps"
	QueueRunning       QueueState = "running"
)

type QueueTask struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Labels         map[string]string `json:"labels"`
	AgentID        float64           `json:"agent_id"`
	Created        float64           `json:"created"`
	PipelineNumber float64           `json:"pipeline_number"`
	AgentName      string            `json:"agent_name"`
}

type QueueInfo struct {
	Pending       []QueueTask `json:"pending"`
	WaitingOnDeps []QueueTask `json:"waiting_on_deps"`
	Running       []QueueTask `json:"running"`
}

type Pipeline struct {
	Number  float64  `json:"number"`
	Branch  string   `json:"branch"`
	Status  *string  `json:"status"`
	Created *float64 `json:"created"`
}

type QueueRow struct {
	Workflow   string
	State      QueueState
	AgeSeconds int64
	Pipeline   float64
	Rank       *int
	AgentName  *string
}

type Options struct {
	Help        bool
	Branch      string
	HasBranch   bool
	Pipeline    float64
	HasPipeline bool
	Repo        string
	HasRepo     bool
}

type apiClient struct {
	server string
	token  string
	http   *http.Client
}

type dependencies struct {
	Getenv        func(string) (string, bool)
	Client        *http.Client
	Now           func() time.Time
	Stdout        io.Writer
	ResolveBranch func(context.Context) (string, error)
}

func parseArgs(args []string) (Options, error) {
	var options Options
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--help" || argument == "-h" {
			options.Help = true
			return options, nil
		}
		if argument != "--branch" && argument != "--pipeline" && argument != "--repo" {
			return Options{}, fmt.Errorf("ci-queue: unknown argument %q\n\n%s", argument, usage)
		}
		index++
		if index >= len(args) || strings.HasPrefix(args[index], "--") {
			return Options{}, fmt.Errorf("ci-queue: %s needs a value\n\n%s", argument, usage)
		}
		value := args[index]
		switch argument {
		case "--branch":
			options.Branch, options.HasBranch = value, true
		case "--repo":
			options.Repo, options.HasRepo = value, true
		case "--pipeline":
			number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || !finite(number) || number <= 0 || number != math.Trunc(number) {
				return Options{}, fmt.Errorf("ci-queue: --pipeline needs a positive integer, got %q", value)
			}
			options.Pipeline, options.HasPipeline = number, true
		}
	}
	return options, nil
}

func parseQueueTask(value any) (QueueTask, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return QueueTask{}, errors.New("queue task is not an object")
	}
	id, ok := object["id"].(string)
	if !ok {
		return QueueTask{}, errors.New("queue task has no id")
	}
	name, ok := object["name"].(string)
	if !ok {
		return QueueTask{}, errors.New("queue task has no name")
	}
	created, ok := finiteNumber(object["created"])
	if !ok {
		return QueueTask{}, errors.New("queue task has no created timestamp")
	}
	pipeline, ok := finiteNumber(object["pipeline_number"])
	if !ok || pipeline <= 0 || pipeline != math.Trunc(pipeline) {
		return QueueTask{}, errors.New("queue task has no positive pipeline_number")
	}
	agentID, ok := finiteNumber(object["agent_id"])
	if !ok {
		return QueueTask{}, errors.New("queue task has no agent_id")
	}
	labels, err := parseLabels(object["labels"])
	if err != nil {
		return QueueTask{}, err
	}
	agentName, _ := object["agent_name"].(string)
	return QueueTask{ID: id, Name: name, Labels: labels, AgentID: agentID, Created: created, PipelineNumber: pipeline, AgentName: agentName}, nil
}

func parseLabels(value any) (map[string]string, error) {
	if value == nil {
		return map[string]string{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("queue task labels are not an object")
	}
	labels := make(map[string]string, len(object))
	for key, raw := range object {
		if label, ok := raw.(string); ok {
			labels[key] = label
		}
	}
	return labels, nil
}

func parseQueueInfo(value any) (QueueInfo, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return QueueInfo{}, errors.New("queue info is not an object")
	}
	var info QueueInfo
	for _, bucket := range []struct {
		name string
		rows *[]QueueTask
	}{
		{name: "pending", rows: &info.Pending},
		{name: "waiting_on_deps", rows: &info.WaitingOnDeps},
		{name: "running", rows: &info.Running},
	} {
		raw, exists := object[bucket.name]
		if !exists {
			return QueueInfo{}, fmt.Errorf("queue info has no %s bucket", bucket.name)
		}
		if raw == nil {
			*bucket.rows = []QueueTask{}
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			return QueueInfo{}, fmt.Errorf("queue info %s bucket is not an array", bucket.name)
		}
		*bucket.rows = make([]QueueTask, 0, len(items))
		for _, item := range items {
			task, err := parseQueueTask(item)
			if err != nil {
				return QueueInfo{}, err
			}
			*bucket.rows = append(*bucket.rows, task)
		}
	}
	return info, nil
}

func parsePipelines(value any) ([]Pipeline, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("pipeline list is not an array")
	}
	pipelines := make([]Pipeline, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("pipeline is not an object")
		}
		number, ok := finiteNumber(object["number"])
		if !ok || number <= 0 || number != math.Trunc(number) {
			return nil, errors.New("pipeline has no positive number")
		}
		branch, ok := object["branch"].(string)
		if !ok {
			return nil, errors.New("pipeline has no branch")
		}
		pipeline := Pipeline{Number: number, Branch: branch}
		if status, ok := object["status"].(string); ok {
			pipeline.Status = new(status)
		}
		if created, ok := finiteNumber(object["created"]); ok {
			pipeline.Created = new(created)
		}
		pipelines = append(pipelines, pipeline)
	}
	return pipelines, nil
}

func finite(number float64) bool {
	return !math.IsNaN(number) && !math.IsInf(number, 0)
}

func finiteNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok && finite(number)
}

func rankPending(info QueueInfo, task QueueTask) *int {
	targetIndex := -1
	for index, candidate := range info.Pending {
		if candidate.ID == task.ID {
			targetIndex = index
			break
		}
	}
	if targetIndex < 0 {
		return nil
	}

	rank := 1
	// Match JavaScript localeCompare for names with the same creation time.
	englishCollator := collate.New(language.English)
	for index, candidate := range info.Pending {
		before := candidate.Created < task.Created
		if candidate.Created == task.Created {
			comparison := englishCollator.CompareString(candidate.Name, task.Name)
			before = comparison < 0 || comparison == 0 && index < targetIndex
		}
		if before {
			rank++
		}
	}
	return &rank
}

func rowsForPipelines(info QueueInfo, pipelineNumbers []float64, nowSeconds float64, repo string) []QueueRow {
	wanted := make(map[float64]struct{}, len(pipelineNumbers))
	for _, number := range pipelineNumbers {
		wanted[number] = struct{}{}
	}
	rows := make([]QueueRow, 0)
	buckets := []struct {
		state QueueState
		rows  []QueueTask
	}{{QueuePending, info.Pending}, {QueueWaitingOnDeps, info.WaitingOnDeps}, {QueueRunning, info.Running}}
	for _, bucket := range buckets {
		for _, task := range bucket.rows {
			if _, ok := wanted[task.PipelineNumber]; !ok || task.Labels["repo"] != repo {
				continue
			}
			age := max(int64(math.Floor(nowSeconds-task.Created)), 0)
			row := QueueRow{Workflow: task.Name, State: bucket.state, AgeSeconds: age, Pipeline: task.PipelineNumber}
			if bucket.state == QueuePending {
				row.Rank = rankPending(info, task)
			}
			if bucket.state == QueueRunning && task.AgentName != "" {
				row.AgentName = new(task.AgentName)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func escapeCell(value string) string {
	var output strings.Builder
	for _, char := range value {
		if char > 0x1f && char != 0x7f {
			output.WriteRune(char)
			continue
		}
		switch char {
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		default:
			fmt.Fprintf(&output, `\x%02x`, char)
		}
	}
	return output.String()
}

func renderTable(rows []QueueRow, scope string) string {
	if len(rows) == 0 {
		return fmt.Sprintf("no queued tasks for %s\n", escapeCell(scope))
	}
	var output strings.Builder
	output.WriteString("WORKFLOW\tSTATE\tAGE\tPIPELINE\tRANK\tAGENT\n")
	for _, row := range rows {
		output.WriteString(escapeCell(row.Workflow))
		output.WriteByte('\t')
		output.WriteString(string(row.State))
		output.WriteByte('\t')
		output.WriteString(strconv.FormatInt(row.AgeSeconds, 10))
		output.WriteString("s\t")
		output.WriteString(strconv.FormatFloat(row.Pipeline, 'f', -1, 64))
		output.WriteByte('\t')
		if row.Rank == nil {
			output.WriteByte('-')
		} else {
			output.WriteString(strconv.Itoa(*row.Rank))
		}
		output.WriteByte('\t')
		if row.AgentName == nil {
			output.WriteByte('-')
		} else {
			output.WriteString(escapeCell(*row.AgentName))
		}
		output.WriteByte('\n')
	}
	return output.String()
}

func newAPI(server, token string, client *http.Client) apiClient {
	if client == nil {
		client = http.DefaultClient
	}
	return apiClient{server: strings.TrimRight(server, "/"), token: token, http: client}
}

func (client apiClient) fetchJSON(ctx context.Context, path string) (any, error) {
	requestContext, cancel := context.WithTimeout(ctx, timeoutSeconds*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, client.server+path, nil)
	if err != nil {
		return nil, fmt.Errorf("ci-queue: creating request for %s: %w", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("ci-queue: request %s: %w", path, err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("ci-queue: reading WP API response for %s: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("ci-queue: closing WP API response for %s: %w", path, closeErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		statusText := strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)+" ")
		return nil, fmt.Errorf("ci-queue: WP API %d %s for %s", response.StatusCode, statusText, path)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("ci-queue: decoding WP API response for %s: %w", path, err)
	}
	return value, nil
}

func resolveRepoID(ctx context.Context, client apiClient, repo string) (float64, error) {
	body, err := client.fetchJSON(ctx, "/api/repos/lookup/"+url.PathEscape(repo))
	if err != nil {
		return 0, err
	}
	object, ok := body.(map[string]any)
	if !ok {
		return 0, errors.New("ci-queue: invalid repo lookup response")
	}
	id, ok := finiteNumber(object["id"])
	if !ok {
		return 0, errors.New("ci-queue: invalid repo lookup response")
	}
	return id, nil
}

func pipelineNumbersForBranch(ctx context.Context, client apiClient, repoID float64, branch string) ([]float64, error) {
	numbers := make([]float64, 0)
	for page := 1; page <= pipelinesMaxPages; page++ {
		branchQuery := strings.ReplaceAll(url.QueryEscape(branch), "+", "%20")
		path := fmt.Sprintf("/api/repos/%s/pipelines?branch=%s&page=%d&perPage=%d", strconv.FormatFloat(repoID, 'f', -1, 64), branchQuery, page, pipelinesPageSize)
		body, err := client.fetchJSON(ctx, path)
		if err != nil {
			return nil, err
		}
		pipelines, err := parsePipelines(body)
		if err != nil {
			return nil, err
		}
		for _, pipeline := range pipelines {
			if pipeline.Branch == branch {
				numbers = append(numbers, pipeline.Number)
			}
		}
		if len(pipelines) < pipelinesPageSize {
			break
		}
	}
	return numbers, nil
}

func verifyPipelineForRepo(ctx context.Context, client apiClient, repoID, pipelineNumber float64, repo string) error {
	path := fmt.Sprintf("/api/repos/%s/pipelines/%s", strconv.FormatFloat(repoID, 'f', -1, 64), strconv.FormatFloat(pipelineNumber, 'f', -1, 64))
	body, err := client.fetchJSON(ctx, path)
	if err != nil {
		if strings.Contains(err.Error(), "WP API 404") {
			return fmt.Errorf("ci-queue: pipeline %s was not found in repository %s: %w", strconv.FormatFloat(pipelineNumber, 'f', -1, 64), repo, err)
		}
		return err
	}
	pipelines, err := parsePipelines([]any{body})
	if err != nil {
		return fmt.Errorf("ci-queue: invalid pipeline %s response for repository %s: %w", strconv.FormatFloat(pipelineNumber, 'f', -1, 64), repo, err)
	}
	if len(pipelines) == 0 || pipelines[0].Number != pipelineNumber {
		return fmt.Errorf("ci-queue: pipeline %s response did not match the requested pipeline in repository %s", strconv.FormatFloat(pipelineNumber, 'f', -1, 64), repo)
	}
	return nil
}

func fetchQueueInfo(ctx context.Context, client apiClient) (QueueInfo, error) {
	body, err := client.fetchJSON(ctx, "/api/queue/info")
	if err != nil {
		return QueueInfo{}, err
	}
	return parseQueueInfo(body)
}

func gitBranch(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, "git", "branch", "--show-current")
	output, err := command.Output()
	branch := strings.TrimSpace(string(output))
	if err != nil || branch == "" {
		return "", errors.New("ci-queue: could not determine the current branch — pass --branch or --pipeline")
	}
	return branch, nil
}

func readWoodpeckerToken(getenv func(string) (string, bool)) string {
	path, _ := getenv("WOODPECKER_TOKEN_FILE")
	if token, ok := credentialfile.Read(path); ok {
		return token
	}
	token, _ := getenv("WOODPECKER_TOKEN")
	return token
}
func resolveBranch(getenv func(string) (string, bool), resolver func(context.Context) (string, error), ctx context.Context) (string, error) {
	if branch, ok := getenv("WOODPECKER_BRANCH"); ok && branch != "" {
		return branch, nil
	}
	if resolver == nil {
		resolver = gitBranch
	}
	return resolver(ctx)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if getenv == nil {
		getenv = os.Getenv
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = io.Discard
	}
	err := runWithDependencies(ctx, args, dependencies{
		Getenv: func(name string) (string, bool) {
			value := getenv(name)
			return value, value != ""
		},
		Client: http.DefaultClient,
		Stdout: stdout,
	})
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
		return 2
	}
	return 1
}

func runWithDependencies(ctx context.Context, args []string, deps dependencies) error {
	options, err := parseArgs(args)
	if err != nil {
		return err
	}
	stdout := deps.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if options.Help {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	getenv := deps.Getenv
	if getenv == nil {
		getenv = func(name string) (string, bool) {
			value := os.Getenv(name)
			return value, value != ""
		}
	}
	server, _ := getenv("WOODPECKER_SERVER")
	token := readWoodpeckerToken(getenv)
	if server == "" || token == "" {
		return errors.New("ci-queue: WOODPECKER_SERVER and WOODPECKER_TOKEN are required")
	}
	client := newAPI(server, token, deps.Client)
	repo := ""
	if options.HasRepo {
		repo = options.Repo
	} else if value, ok := getenv("WOODPECKER_REPO"); ok && value != "" {
		repo = value
	}
	if repo == "" {
		return errors.New("ci-queue: --repo or WOODPECKER_REPO is required")
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	nowSeconds := float64(now().Unix())
	repoID, err := resolveRepoID(ctx, client, repo)
	if err != nil {
		return err
	}
	var pipelineNumbers []float64
	var scope string
	if options.HasPipeline {
		if err := verifyPipelineForRepo(ctx, client, repoID, options.Pipeline, repo); err != nil {
			return err
		}
		pipelineNumbers = []float64{options.Pipeline}
		scope = "pipeline " + strconv.FormatFloat(options.Pipeline, 'f', -1, 64)
	} else {
		branch := options.Branch
		if !options.HasBranch {
			branch, err = resolveBranch(getenv, deps.ResolveBranch, ctx)
			if err != nil {
				return err
			}
		}
		pipelineNumbers, err = pipelineNumbersForBranch(ctx, client, repoID, branch)
		if err != nil {
			return err
		}
		scope = "branch " + branch
	}
	info, err := fetchQueueInfo(ctx, client)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, renderTable(rowsForPipelines(info, pipelineNumbers, nowSeconds, repo), scope))
	return err
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
