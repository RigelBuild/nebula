package main

const prStateClosed = "closed"

type StatusRow struct {
	Repo      string `json:"repo"`
	PR        int    `json:"pr"`
	Bookmark  string `json:"bookmark"`
	State     string `json:"state"`
	Conflicts string `json:"conflicts"`
	Hooks     string `json:"hooks"`
	Review    string `json:"review"`
	Approval  string `json:"approval"`
	CI        string `json:"ci"`
	Next      string `json:"next"`
}

func nextAction(row StatusRow, parentLanded, mainRedEligible, promotable bool) string {
	switch {
	case row.State == "merged" || row.State == prStateClosed:
		return "rjc retire"
	case parentLanded || row.Conflicts == "conflicting":
		return "rjc sync"
	case (row.Hooks == "skipped" || row.Hooks == "partial") && mainRedEligible:
		return "rjc ready --main-red"
	case row.Hooks != "pass" && row.Hooks != "main-red" && !promotable:
		return "rjc submit"
	case row.Review != string(markerAllClear):
		return "run the review loop"
	case row.State == "ready":
		return "hand off"
	default:
		return "rjc ready"
	}
}
