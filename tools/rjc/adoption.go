package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/samber/oops"
)

func (r *repository) checkAdoptionPRs(ctx context.Context, rev string) error {
	commits, err := r.revisions(ctx, rev)
	if err != nil {
		return err
	}
	if err := prepareForge(ctx, r); err != nil {
		return err
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	var repo struct {
		FullName string `json:"full_name"`
	}
	if err := s.forge.api(ctx, http.MethodGet, "/repos/"+r.profile.Name, nil, &repo); err != nil {
		return oops.Wrapf(err, "verify adoption repository access")
	}
	if !strings.EqualFold(repo.FullName, r.profile.Name) {
		return oops.Errorf("adoption repository identity was not confirmed")
	}
	for _, sha := range commits {
		var prs []struct{ Head struct{ Ref string } }
		path := "/repos/" + r.profile.Name + "/commits/" + sha + "/pulls?per_page=100"
		if err := s.forge.api(ctx, http.MethodGet, path, nil, &prs); err != nil {
			failure, ok := errors.AsType[*forgeAPIError](err)
			if ok && (failure.status == http.StatusNotFound || failure.status == http.StatusUnprocessableEntity) && failure.message == "No commit found for SHA: "+sha {
				continue
			}
			return oops.Wrapf(err, "inspect adoption commit PRs")
		}
		if len(prs) >= 100 {
			return oops.Errorf("adoption commit PR associations exceed the single-page bound")
		}
		for _, pr := range prs {
			if !r.keys.Owned(r.codename, pr.Head.Ref) {
				return refusal("new", "remote-parent", "--at reaches work associated with another caller's PR: "+pr.Head.Ref, "rjc new --on <bookmark> --at <rev> to select and record the parent explicitly")
			}
		}
	}
	return nil
}
