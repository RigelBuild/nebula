package main

import "context"

func (r *repository) prepareCandidate(ctx context.Context, bookmark, title, bodyFile, skip string) (candidate, error) {
	template, err := r.template(ctx)
	if err != nil {
		return candidate{}, err
	}
	authored, err := metadataBody(template, bodyFile, nil)
	if err != nil {
		return candidate{}, err
	}
	return r.prepareResolvedCandidate(ctx, bookmark, "", title, authored, template, skip)
}
