package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/config"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
	"github.com/spf13/cobra"
)

func newInventoryCmd() *cobra.Command {
	var host, configPath, previousPath, deltaPath, outPath string
	command := &cobra.Command{
		Use:   "inventory",
		Short: "Inventory transcript source files",
		Args:  cobra.NoArgs,
		PreRunE: func(command *cobra.Command, _ []string) error {
			if host == "" || configPath == "" || outPath == "" {
				return errors.New("inventory requires host, config and out")
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			return runInventory(host, configPath, previousPath, deltaPath, outPath)
		},
	}
	command.Flags().StringVar(&host, "host", "", "host name from the archive config")
	command.Flags().StringVar(&configPath, "config", "", "archive CUE config path (required)")
	command.Flags().StringVar(&previousPath, "previous", "", "previous inventory JSONL path")
	command.Flags().StringVar(&deltaPath, "delta", "", "scan delta output path")
	command.Flags().StringVar(&outPath, "out", "", "inventory output path")
	return command
}

func runInventory(host, configPath, previousPath, deltaPath, outPath string) error {
	var previous *inventory.Inventory
	if previousPath != "" {
		var err error
		previous, err = readInventory(previousPath)
		if err != nil {
			return fmt.Errorf("read previous inventory: %w", err)
		}
	}
	current, delta, empty, err := walkHost(host, configPath, previous)
	if err != nil {
		return err
	}
	if empty {
		return writeEmptyInventory(outPath)
	}
	output, err := os.Create(outPath) //nolint:gosec // Operator-supplied output path.
	if err != nil {
		return fmt.Errorf("create inventory output: %w", err)
	}
	if err := errors.Join(writeInventory(output, current), output.Close()); err != nil {
		return fmt.Errorf("write inventory output: %w", err)
	}
	if deltaPath != "" {
		return writeDelta(deltaPath, delta)
	}
	return nil
}

// hostWalk loads a host's walk set rooted at HOME; empty reports an empty-state host with no sources.
func hostWalk(host, configPath string) (walk config.Walk, empty bool, err error) {
	loaded, err := config.Load(configPath)
	if err != nil {
		return walk, false, err
	}
	walk, err = loaded.Walk(host)
	if err != nil {
		return walk, false, err
	}
	if walk.State == "empty" && len(walk.Sources) == 0 {
		return walk, true, nil
	}
	walk.Root, err = os.UserHomeDir()
	if err != nil {
		return walk, false, fmt.Errorf("resolve home directory: %w", err)
	}
	return walk, false, nil
}

// walkHost inventories a host's configured sources under HOME; empty reports an empty-state host with no sources.
func walkHost(host, configPath string, previous *inventory.Inventory) (current inventory.Inventory, delta inventory.Delta, empty bool, err error) {
	walk, empty, err := hostWalk(host, configPath)
	if err != nil || empty {
		return current, delta, empty, err
	}
	current, delta, err = inventory.Walk(walk, previous)
	return current, delta, false, err
}

func readInventory(path string) (*inventory.Inventory, error) {
	file, err := os.Open(path) //nolint:gosec // Path is an operator-supplied CLI flag.
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	result := inventory.Inventory{Lines: []inventory.Line{}, Dangling: []inventory.Dangling{}, Excluded: []inventory.Excluded{}, Errors: []inventory.AssignError{}}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row struct {
			Error          *inventory.AssignError `json:"error"`
			Dangling       *inventory.Dangling    `json:"dangling"`
			Excluded       *inventory.Excluded    `json:"excluded"`
			OutsideTargets *int                   `json:"outsideTargets"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, err
		}
		var line inventory.Line
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, err
		}
		if line.Path != "" {
			result.Lines = append(result.Lines, line)
		}
		if row.Error != nil {
			result.Errors = append(result.Errors, *row.Error)
		}
		if row.Dangling != nil {
			result.Dangling = append(result.Dangling, *row.Dangling)
		}
		if row.Excluded != nil {
			result.Excluded = append(result.Excluded, *row.Excluded)
		}
		if row.OutsideTargets != nil {
			result.OutsideTargets = *row.OutsideTargets
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &result, nil
}

func writeInventory(writer io.Writer, inv inventory.Inventory) error {
	encoder := json.NewEncoder(writer)
	for _, line := range inv.Lines {
		if err := encoder.Encode(line); err != nil {
			return err
		}
	}
	for _, excluded := range inv.Excluded {
		if err := encoder.Encode(struct {
			Excluded inventory.Excluded `json:"excluded"`
		}{Excluded: excluded}); err != nil {
			return err
		}
	}
	for _, dangling := range inv.Dangling {
		if err := encoder.Encode(struct {
			Dangling inventory.Dangling `json:"dangling"`
		}{Dangling: dangling}); err != nil {
			return err
		}
	}
	for _, assignment := range inv.Errors {
		if err := encoder.Encode(struct {
			Error inventory.AssignError `json:"error"`
		}{Error: assignment}); err != nil {
			return err
		}
	}
	return encoder.Encode(struct {
		OutsideTargets int `json:"outsideTargets"`
	}{OutsideTargets: inv.OutsideTargets})
}

// writeEmptyInventory records an empty-state host with no sources as a zero-line inventory.
func writeEmptyInventory(outPath string) error {
	output, err := os.Create(outPath) //nolint:gosec // Operator-supplied output path.
	if err != nil {
		return fmt.Errorf("create inventory output: %w", err)
	}
	empty := inventory.Inventory{Lines: []inventory.Line{}, Dangling: []inventory.Dangling{}, Excluded: []inventory.Excluded{}, Errors: []inventory.AssignError{}}
	if err := errors.Join(writeInventory(output, empty), output.Close()); err != nil {
		return fmt.Errorf("write inventory output: %w", err)
	}
	return nil
}

func writeDelta(path string, delta inventory.Delta) error {
	file, err := os.Create(path) //nolint:gosec // Path is an operator-supplied CLI flag.
	if err != nil {
		return fmt.Errorf("create delta output: %w", err)
	}
	encoder := json.NewEncoder(file)
	if err := inventory.WriteDelta(encoder, delta); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("encode delta: %w", err), closeErr)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close delta output: %w", err)
	}
	return nil
}
