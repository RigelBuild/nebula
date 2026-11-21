package inventory

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// MarshalJSON writes the inventory line with a hex SHA-256.
func (line Line) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Root       string `json:"root"`
		Path       string `json:"path"`
		Type       string `json:"type"`
		Size       int64  `json:"size"`
		MtimeNs    int64  `json:"mtimeNs"`
		SHA256     string `json:"sha256"`
		Record     string `json:"record"`
		SessionID  string `json:"sessionId,omitempty"`
		Class      string `json:"class"`
		LinkTarget string `json:"linkTarget,omitempty"`
	}{
		Root: line.Root, Path: line.Path, Type: line.Type, Size: line.Size, MtimeNs: line.MtimeNs,
		SHA256: hex.EncodeToString(line.SHA256[:]), Record: line.Record, SessionID: line.SessionID,
		Class: line.Class, LinkTarget: line.LinkTarget,
	})
}

func (line *Line) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Root       string          `json:"root"`
		Path       string          `json:"path"`
		Type       string          `json:"type"`
		Size       int64           `json:"size"`
		MtimeNs    int64           `json:"mtimeNs"`
		SHA256     json.RawMessage `json:"sha256"`
		Record     string          `json:"record"`
		SessionID  string          `json:"sessionId"`
		Class      string          `json:"class"`
		LinkTarget string          `json:"linkTarget"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var digest [32]byte
	var encoded string
	if err := json.Unmarshal(decoded.SHA256, &encoded); err == nil {
		if len(encoded) != 64 {
			return errors.New("sha256 must contain 64 hexadecimal characters")
		}
		if _, err := hex.Decode(digest[:], []byte(encoded)); err != nil {
			return fmt.Errorf("decode sha256: %w", err)
		}
	} else if len(decoded.SHA256) != 0 && string(decoded.SHA256) != "null" {
		if err := json.Unmarshal(decoded.SHA256, &digest); err != nil {
			return fmt.Errorf("decode sha256: %w", err)
		}
	}
	*line = Line{
		Root: decoded.Root, Path: decoded.Path, Type: decoded.Type, Size: decoded.Size,
		MtimeNs: decoded.MtimeNs, SHA256: digest, Record: decoded.Record, SessionID: decoded.SessionID,
		Class: decoded.Class, LinkTarget: decoded.LinkTarget,
	}
	return nil
}
