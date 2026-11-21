// #Layout is the durable-state table for one deployment. The file given to
// --inventory is unified with #Layout and must be concrete.
#Layout: {
	rows: [...#Row] & [_, ...]
	google:   #Google
	journals: #Journals
	// processes are substrings of ps command lines that must be gone after
	// mac stop. {home} stands for the source home.
	processes: [...string & !=""] & [_, ...]
}

#Row: {
	// path is relative to the source home.
	path: string & !="" & !~"^/"
	kind: "copy" | "rebuild" | "retire"
	// dest is the container path of a copy row. Rebuild and retire rows
	// describe their destination in words.
	dest: string & !=""
	if kind == "copy" {
		dest: =~"^/opt/data(/|$)"
	}
	// skip lists paths under path that the walk does not enter. Another row
	// covers them, or they are runtime files.
	skip?: [...string & !=""]
	// discover lists directories under path whose children no row covers.
	// Each such child is reported as an unknown copy root.
	discover?: [...#Discover]
}

#Discover: {
	// dir is relative to the row path. "." is the row path itself.
	dir: string & !=""
	// known lists children of dir that have their own rows. They are not reported.
	known?: [...string & !=""]
}

#Google: {
	// source is the Mac directory holding <source>/<account>/credentials.
	source: =~"^/."
	// dest is the container directory the trees restore to. It sits under /opt/data.
	dest: =~"^/opt/data/."
	accounts: [...string & =~"^[A-Za-z0-9][A-Za-z0-9._-]*$"] & [_, ...]
}

#Journals: {
	// dir is the Mac directory holding the paging journals (*.jsonl).
	dir: =~"^/."
	// names are bare journal file names. They are created empty when the
	// source has none.
	names: [...string & =~"^[^/]+[.]jsonl$"] & [_, ...]
}
