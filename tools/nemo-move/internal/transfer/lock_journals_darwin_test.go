//go:build darwin

package transfer

func useTestJournalLocker() func() { return func() {} }
