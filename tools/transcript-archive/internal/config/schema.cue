package archive

#Source: {
	name: string
	path: string
}

#Exclusion: {
	path: string
}

#Host: {
	state: "measured" | "unmeasured" | "empty"
	sources: [...#Source]
	watch: [...string]
	exclude: [...#Exclusion]
	finalOnly: [...string]
}

hosts: [string]: #Host
