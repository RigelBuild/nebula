package transfer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"unicode/utf8"

	"github.com/RigelBuild/nebula/tools/nemo-move/internal/layout"
)

// rewriteFile verifies source bytes before generating the expected destination entry.
func rewriteFile(source io.Reader, entry ManifestEntry, rewrite bool, tempDir, sourceHome string, lay layout.Layout) (_ *os.File, destination ManifestEntry, resultErr error) {
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, entry, fmt.Errorf("create rewrite temp directory: %w", err)
	}
	input, err := os.CreateTemp(tempDir, "nemo-move-verified-*")
	if err != nil {
		return nil, entry, err
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		resultErr = errors.Join(resultErr, input.Close(), os.Remove(input.Name()))
	}()
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(input, hash), source, entry.Size); err != nil {
		return nil, entry, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return nil, entry, fmt.Errorf("archive digest mismatch at %s", entry.Path)
	}
	if !rewrite {
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return nil, entry, err
		}
		keep = true
		return input, entry, nil
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return nil, entry, err
	}
	text, err := isRewriteText(input)
	if err != nil {
		return nil, entry, err
	}
	if !text {
		if err := rejectBinarySourceHome(input, entry.Path, sourceHome); err != nil {
			return nil, entry, err
		}
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return nil, entry, err
		}
		keep = true
		return input, entry, nil
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return nil, entry, err
	}
	output, err := os.CreateTemp(tempDir, "nemo-move-rewritten-*")
	if err != nil {
		return nil, entry, err
	}
	outputKept := false
	defer func() {
		if !outputKept || resultErr != nil {
			resultErr = errors.Join(resultErr, output.Close(), os.Remove(output.Name()))
		}
	}()
	if err := rewriteText(input, output, sourceHome, lay); err != nil {
		return nil, entry, err
	}
	hash = sha256.New()
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, entry, err
	}
	if _, err := io.Copy(hash, output); err != nil {
		return nil, entry, err
	}
	size, err := output.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, entry, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, entry, err
	}
	entry.Size, entry.SHA256 = size, hex.EncodeToString(hash.Sum(nil))
	if err := input.Close(); err != nil {
		return nil, entry, err
	}
	if err := os.Remove(input.Name()); err != nil {
		return nil, entry, err
	}
	keep = true
	outputKept = true
	return output, entry, nil
}

func isRewriteText(source io.Reader) (bool, error) {
	r := bufio.NewReader(source)
	for {
		value, size, err := r.ReadRune()
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if value == 0 || value == utf8.RuneError && size == 1 {
			return false, nil
		}
	}
}

func rewriteText(source io.Reader, output io.Writer, sourceHome string, lay layout.Layout) error {
	return rewriteTextWithBoundaries(source, output, sourceHome, lay)
}

func rewriteTextWithBoundaries(source io.Reader, output io.Writer, sourceHome string, lay layout.Layout) error {
	r := bufio.NewReader(source)
	w := bufio.NewWriter(output)
	prefixes := homePrefixes(sourceHome, lay)
	for {
		first, err := r.Peek(1)
		if errors.Is(err, io.EOF) {
			return w.Flush()
		}
		if err != nil {
			return err
		}
		matched := false
		if first[0] == '/' {
			for _, prefix := range prefixes {
				data, peekErr := r.Peek(len(prefix.source) + 1)
				if peekErr != nil && !errors.Is(peekErr, io.EOF) {
					return peekErr
				}
				if !bytes.HasPrefix(data, []byte(prefix.source)) {
					continue
				}
				boundary := len(data) == len(prefix.source)
				if len(data) > len(prefix.source) {
					next := data[len(prefix.source)]
					boundary = next == '/' || !isPathByte(next)
				}
				if !boundary {
					continue
				}
				if _, err := r.Discard(len(prefix.source)); err != nil {
					return err
				}
				if _, err := w.WriteString(prefix.dest); err != nil {
					return err
				}
				matched = true
				break
			}
		}
		if !matched {
			b, err := r.ReadByte()
			if err != nil {
				return err
			}
			if err := w.WriteByte(b); err != nil {
				return err
			}
		}
	}
}

func isPathByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.'
}

// homePrefix maps a source path prefix to the container path that replaces it.
type homePrefix struct{ source, dest string }

// homePrefixes lists each copy row as a source prefix under sourceHome with its
// container dest, longest source first. The source home itself comes last and
// falls back to /opt/data.
func homePrefixes(sourceHome string, lay layout.Layout) []homePrefix {
	var prefixes []homePrefix
	for _, row := range lay.Rows {
		if row.Kind == layout.Copy {
			prefixes = append(prefixes, homePrefix{source: sourceHome + "/" + row.Path, dest: row.Dest})
		}
	}
	prefixes = append(prefixes, homePrefix{source: sourceHome, dest: "/opt/data"})
	sort.SliceStable(prefixes, func(i, j int) bool { return len(prefixes[i].source) > len(prefixes[j].source) })
	return prefixes
}

func rejectBinarySourceHome(file *os.File, path, sourceHome string) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(file)
	matched := 0
	for {
		b, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if b == sourceHome[matched] {
			matched++
			if matched == len(sourceHome) {
				return fmt.Errorf("binary file contains source home path: %s", path)
			}
		} else if b == '/' {
			matched = 1
		} else {
			matched = 0
		}
	}
}
