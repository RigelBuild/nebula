package hookshell

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requireLinuxFetchHost skips the fetch tests off Linux. They run a fake uname and nix as shell scripts and use the
// Linux hk archive, so only Linux exercises them.
func requireLinuxFetchHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("fetch tests need Linux: they run a fake uname and nix as shell scripts and use the Linux hk archive")
	}
}

func TestFetch(t *testing.T) {
	requireLinuxFetchHost(t)
	for _, tc := range []struct {
		name, mode, warning string
		wantHK              bool
	}{
		{name: "fetch and cache", wantHK: true},
		{name: "prefetch failure", mode: "fail", warning: "failed to prefetch"},
		{name: "unsupported platform", mode: "unsupported", warning: "unsupported host"},
		{name: "archive without hk", mode: "empty", warning: "lacks an executable hk"},
		{name: "winner before publication", mode: "winner", wantHK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(home, "hk.tar.gz")
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			name := "hk"
			if tc.mode == "empty" {
				name = "other"
			}
			body := []byte("#!/bin/sh\necho ran >> \"$HOME/marker\"\n")
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			pin := hkPins[0]
			uname := "#!/bin/sh\necho " + pin.host + "\n"
			if tc.mode == "unsupported" {
				uname = "#!/bin/sh\necho Plan9 mips\n"
			}
			nix := "#!/bin/sh\ncase \"$*\" in *prefetch-file*) echo prefetch >> \"$HOME/log\"; "
			url := strings.ReplaceAll(pin.url, "${version}", hkVersion)
			nix += fmt.Sprintf("case \" $* \" in *\" --unpack \"*) exit 1;; esac; case \"$*\" in *\"--expected-hash %s\"*\"%s\"*) ;; *) exit 1;; esac; ", pin.hash, url)
			if tc.mode == "fail" {
				nix += "exit 1"
			} else {
				if tc.mode == "winner" {
					cache := "$HOME/rjc-hook/tools/hk-" + hkVersion + "-" + pin.platform
					nix += fmt.Sprintf("mkdir -p \"%s\"; cp \"$HOME/winner\" \"%s/hk\"; ", cache, cache)
				}
				nix += fmt.Sprintf("echo '{\"hash\":\"verified\",\"storePath\":\"%s\"}'", archive)
			}
			nix += ";; esac\n"
			for path, content := range map[string]string{"uname": uname, "nix": nix} {
				if err := os.WriteFile(filepath.Join(bin, path), []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			winner := []byte("#!/bin/sh\necho winner >> \"$HOME/marker\"\n")
			if err := os.WriteFile(filepath.Join(home, "winner"), winner, 0o755); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cmd := exec.Command("sh", "-c", "set -e\n"+Fetch()+"\nif [ -n \"$hk_dir\" ]; then \"$hk_dir/hk\"; fi\n")
				cmd.Env = append(os.Environ(), "HOME="+home, "TMPDIR="+home, "PATH="+bin+":"+os.Getenv("PATH"))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("fetch: %v: %s", err, out)
				}
				if tc.warning != "" && !strings.Contains(string(out), tc.warning) {
					t.Fatalf("missing warning %q: %s", tc.warning, out)
				}
			}
			marker, err := os.ReadFile(filepath.Join(home, "marker"))
			wantMarker := "ran"
			if tc.mode == "winner" {
				wantMarker = "winner"
			}
			if tc.wantHK && (err != nil || strings.Count(string(marker), wantMarker) != 2) {
				t.Fatalf("hk did not run twice: %q %v", marker, err)
			}
			if !tc.wantHK && !os.IsNotExist(err) {
				t.Fatalf("hk unexpectedly ran: %q %v", marker, err)
			}
			if tc.name == "fetch and cache" {
				log, err := os.ReadFile(filepath.Join(home, "log"))
				if err != nil || strings.Count(string(log), "prefetch") != 1 {
					t.Fatalf("cache missed: %q %v", log, err)
				}
			}
			matches, err := filepath.Glob(filepath.Join(home, "hk.*"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("temporary directory leaked: %v %v", matches, err)
			}
		})
	}
}
