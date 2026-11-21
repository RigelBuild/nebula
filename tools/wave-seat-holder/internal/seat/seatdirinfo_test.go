package seat

// fakeSeatDir is an open seat directory in the fake filesystem. WriteFile records the file the holder wrote,
// under the same path and mode the production path uses, and returns the scripted write error.
type fakeSeatDir struct {
	dir     string
	fake    *fakeHolder
	options fakeOptions
}

func (f fakeSeatDir) WriteFile(name string, data []byte) error {
	if f.options.writeErr != nil {
		return f.options.writeErr
	}
	f.fake.writes = append(f.fake.writes, recordedWrite{path: f.dir + "/" + name, data: append([]byte(nil), data...), perm: 0o600})
	return nil
}

func (f fakeSeatDir) Close() error { return nil }
