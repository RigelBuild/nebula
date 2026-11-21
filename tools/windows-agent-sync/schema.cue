// #Layout names every repo-relative source path the tool reads. Paths use
// forward slashes and must stay inside --repo-root; loadLayout enforces that.
#Layout: {
	// Link table, relative to --repo-root.
	linkTable:  string & !=""
	pins:       string & !=""
	nebulaLock: string & !=""
	mcp:        string & !=""
	// Secret map evaluated by NixOS-WSL.
	secretMap: string & !=""
}

// #Link maps a repo-relative source to a destination. A %APPDATA%/ prefix
// roots the destination in the Roaming profile; any other destination is
// relative to the user profile.
#Link: {
	src: string & !=""
	dst: string & !=""
}

#LinkTable: {
	links: [...#Link]
}
