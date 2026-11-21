// #Config names every deployment value the relauncher reads. Each host supplies one file, passed
// as --config. The source carries no default, so a missing or misspelled field is refused at load.
#Config: {
	// Name of the root seat that the relauncher recovers, e.g. "foreman".
	rootSeat: string & !=""
	// Cotal space that holds the wave, e.g. "main".
	cotalSpace: string & !=""
	// Seat endpoint kind in cotal's endpoint table, e.g. "agent".
	endpointKind: string & !=""
	// Zellij session the root seat's viewer attaches to, e.g. "viewer".
	zellijSession: string & !=""
	// systemd user unit that runs the zellij server.
	zellijUnit: string & !=""
	// systemd user unit that runs the cotal manager.
	cotalManagerUnit: string & !=""
	// systemd user unit that runs this relauncher.
	relaunchUnit: string & !=""
	// systemd user slice that holds every wave agent process.
	waveSlice: string & !=""
	// Desired seat list file, e.g. ~/agents/state/wave-seats.json.
	seatsPath: string & !=""
	// Directory that holds the relauncher's state, hold, manifest and restart-request files.
	stateDir: string & !=""
	// User home directory. Session paths are derived from a cwd relative to it.
	homeDir: string & !=""
	// Directory tree that holds the omp session files, e.g. ~/.omp/agent/sessions.
	sessionsDir: string & !=""
	// Host names a seat's host field may name. An empty list refuses every host.
	hosts: [...string]
	// Path to the cotal manager log, cited in the relaunch prompt.
	managerLogPath: string & !=""
	// Journal units named in the relaunch prompt. At least one.
	journalUnits: [_, ...string]
	// Issue reference that the relaunch prompt cites, e.g. "RIG-0001".
	relaunchIssue: string & !=""
	// Known-cause text cited in the relaunch prompt, e.g. "zellij server out of memory".
	knownCause: string & !=""
	// Owner of the cause report, named in the relaunch prompt, e.g. "harness".
	causeOwner: string & !=""
	// Seat CLI command the relaunch prompt tells the root to run, e.g. "wave-seats".
	seatCLI: string & !=""
	// Command that spawns a fresh agent session, e.g. "spawn-agent".
	spawnCommand: string & !=""
	// Seat holder CLI whose list command reports the live holders, e.g. "wave-seat-holder".
	seatHolderCLI: string & !=""
}
