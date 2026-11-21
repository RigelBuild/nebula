package main

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/samber/oops"
)

type UpstreamRecord struct {
	Repo     string `json:"repo"`
	Release  string `json:"release"`
	Upstream string `json:"upstream"`
	Change   string `json:"change"`
}

func (s fileBaseline) LoadUpstream(bookmark string) (UpstreamRecord, bool, error) {
	path, err := s.path(bookmark, ".upstream.json")
	if err != nil {
		return UpstreamRecord{}, false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // The state path is validated by fileBaseline.path.
	if errors.Is(err, os.ErrNotExist) {
		return UpstreamRecord{}, false, nil
	}
	if err != nil {
		return UpstreamRecord{}, false, oops.Wrapf(err, "read upstream record")
	}
	var rec UpstreamRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return UpstreamRecord{}, false, oops.Wrapf(err, "decode upstream record")
	}
	return rec, true, nil
}

func (s fileBaseline) SaveUpstream(bookmark string, rec UpstreamRecord) error {
	path, err := s.path(bookmark, ".upstream.json")
	if err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return oops.Wrapf(err, "encode upstream record")
	}
	return atomicWrite(path, string(data))
}
