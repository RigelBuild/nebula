package watchdog

import (
	"encoding/json"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

// loadState reads the persisted state; an absent or invalid file reads as nil.
// stateFile is the persisted watch state, under the capture root.
const stateFile = "watch-state.json"

func loadState(io seam.ServerView, statePath string) *watchState {
	text, err := io.ReadText(statePath)
	if err != nil {
		return nil
	}
	state, ok := parseWatchState([]byte(text))
	if !ok {
		return nil
	}
	return state
}

// saveState writes the state as JSON under the field names parseWatchState reads.
func saveState(io seam.FileStore, statePath string, state watchState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return io.WriteText(statePath, string(data))
}
