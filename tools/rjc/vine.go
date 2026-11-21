package main

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/samber/oops"
)

type JJVine interface {
	Submit(ctx context.Context, bookmark, bodyPath string, noHooks, draft bool, log io.Writer) error
}
type vine struct {
	repo *repository
	line []Change
}

func (v vine) Submit(ctx context.Context, bookmark, bodyPath string, noHooks, draft bool, log io.Writer) error {
	if !v.repo.keys.Owned(v.repo.codename, bookmark) {
		return refusal("jj-vine", "ownership", "bookmark is not owned by the caller", "use the caller's codename bookmark")
	}
	var selected *Change
	for i := range v.line {
		if v.line[i].Bookmark == bookmark {
			selected = &v.line[i]
			break
		}
	}
	if selected == nil {
		return refusal("jj-vine", "unvalidated-line", "bookmark has not passed stack preflight", "run rjc submit before invoking the engine")
	}
	if err := v.repo.checkEngineSelection(ctx, *selected, true); err != nil {
		return err
	}
	base := strings.TrimSuffix(selected.Base, "@origin")
	const disabled = "false"
	settings := []workspaceSetting{
		{key: "jj-vine.defaultBaseBranch", value: base},
		{key: "jj-vine.remoteName", value: originRemote},
		{key: "jj-vine.description.singleRevision", value: "file(" + bodyPath + ")"},
		{key: "jj-vine.description.multipleRevisions", value: "file(" + bodyPath + ")"},
		{key: "jj-vine.title.syncSingleRevision", value: disabled},
		{key: "jj-vine.title.syncMultipleRevisions", value: disabled},
		{key: "jj-vine.description.sync", value: disabled},
		{key: "jj-vine.openAsDraft", value: strconv.FormatBool(draft)},
	}
	args := []string{submitCommandName, quoteRev(bookmark)}
	if noHooks {
		args = append(args, "--no-hooks")
	}
	cmd := v.repo.execCommand(ctx, "jj-vine", args...)
	cmd.Dir = v.repo.root
	cmd.Env = v.repo.childEnv(nil)
	cmd.Stdout = log
	cmd.Stderr = log
	return v.repo.withVineConfig(ctx, settings, func() error {
		return oops.Wrapf(cmd.Run(), "jj-vine submit failed; inspect the full log before rerunning")
	})
}
