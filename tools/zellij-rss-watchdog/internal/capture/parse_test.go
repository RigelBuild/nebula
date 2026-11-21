package capture

import (
	"reflect"
	"strings"
	"testing"
)

func TestSSParsingJoinsClientPeerInodesToServerLocalInodes(t *testing.T) {
	server := `u_str ESTAB 0 0 /run/user/1000/zellij/example-session 183845950 * 183833990 users:(("zellij daemon",pid=123,fd=29))`
	client := `u_str ESTAB 0 0 * 183833990 * 183845950 users:(("zellij helper",pid=456,fd=5))`
	unrelated := `u_str ESTAB 0 0 * 183900001 * 183900002 users:(("bash",pid=789,fd=3))`
	serverLines, peers := parseSsPeers(server+"\n"+client+"\n"+unrelated, "123")
	if !reflect.DeepEqual(serverLines, []string{server}) {
		t.Fatalf("serverLines = %v", serverLines)
	}
	if !reflect.DeepEqual(peers, []string{"456"}) {
		t.Fatalf("peerPIDs = %v, want [456]", peers)
	}
}

func TestSSParserIgnoresMalformedFieldRows(t *testing.T) {
	malformed := `u_str ESTAB 0 0 * 123 users:(("zellij",pid=123,fd=4))`
	serverLines, peers := parseSsPeers(malformed, "123")
	if !reflect.DeepEqual(serverLines, []string{malformed}) || len(peers) != 0 {
		t.Fatalf("got serverLines=%v peerPIDs=%v", serverLines, peers)
	}
	serverLines, peers = parseSsPeers("", "123")
	if len(serverLines) != 0 || len(peers) != 0 {
		t.Fatalf("empty input gave serverLines=%v peerPIDs=%v", serverLines, peers)
	}
}

func TestMappingSelectionChoosesFourLargestAnonymousRWAndCapsEachRange(t *testing.T) {
	smaps := strings.Join([]string{
		"10000000-35800000 rw-p 00000000 00:00 0\nSize: 614400 kB",
		"40000000-46400000 rw-p 00000000 00:00 0 [heap]\nSize: 102400 kB",
		"50000000-5a000000 rw-p 00000000 00:00 0\nSize: 163840 kB",
		"60000000-65000000 rw-p 00000000 00:00 0\nSize: 81920 kB",
		"70000000-74000000 rw-p 00000000 00:00 0\nSize: 65536 kB",
		"80000000-8c800000 rw-p 00000000 00:00 0 /tmp/file\nSize: 204800 kB",
	}, "\n")
	want := []mappingRange{
		{start: "10000000", end: "30000000"},
		{start: "50000000", end: "5a000000"},
		{start: "40000000", end: "46400000"},
		{start: "60000000", end: "65000000"},
	}
	if got := mappingRanges(smaps); !reflect.DeepEqual(got, want) {
		t.Fatalf("mappingRanges = %v, want %v", got, want)
	}
}
