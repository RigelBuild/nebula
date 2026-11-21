package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type State struct {
	Version          int    `json:"version"`
	State            string `json:"state"`
	Host             string `json:"host"`
	InstanceID       string `json:"instanceId,omitempty"`
	Reason           string `json:"reason,omitempty"`
	StopLoginSession string `json:"stopLoginSession,omitempty"`
	ManagerPID       int    `json:"managerPid,omitempty"`
	ManagerToken     string `json:"managerToken,omitempty"`
}

type Status struct {
	State      string `json:"state"`
	Host       string `json:"host"`
	InstanceID string `json:"instanceId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func validateState(state State) error {
	if state.Version != 1 || state.Host == "" {
		return errors.New("invalid state version or host")
	}
	switch state.State {
	case "stopped", "starting", "ready", "stopping", "blocked":
		return nil
	}
	return fmt.Errorf("invalid host state %q", state.State)
}

func Load(path string) (State, error) {
	//nolint:gosec // The operator configures the state file path.
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read host state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode host state: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return State{}, errors.New("invalid trailing host state")
	}
	if err := validateState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func Save(path string, state State) (err error) {
	if err := validateState(state); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode host state: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}
	temp := file.Name()
	defer func() {
		if err != nil {
			if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary state: %w", removeErr))
			}
		}
	}()
	if _, err = file.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write host state: %w", err), file.Close())
	}
	if err = file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync host state: %w", err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close host state: %w", err)
	}
	if err = os.Rename(temp, path); err != nil {
		return fmt.Errorf("replace host state: %w", err)
	}
	//nolint:gosec // The directory comes from the operator-configured state path.
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	if err = directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync state directory: %w", err), directory.Close())
	}
	if err = directory.Close(); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}
