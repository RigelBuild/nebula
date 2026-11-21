//go:build !linux

package main

import "errors"

// errLinuxOnly is returned by every filesystem call off Linux. The image is a
// Linux runner artifact; other platforms only need the package to compile.
var errLinuxOnly = errors.New("rjc-gates-image is linux only")

// realFS mirrors the Linux implementation's fields so main.go compiles on
// every platform. Every method fails with errLinuxOnly.
type realFS struct {
	mkfsPath  string
	mountinfo string
}

func (realFS) Exists(string) (bool, error)           { return false, errLinuxOnly }
func (realFS) FilesystemType(string) (string, error) { return "", errLinuxOnly }
func (realFS) NoCOW(string) (bool, error)            { return false, errLinuxOnly }
func (realFS) CreateEmpty(string) error              { return errLinuxOnly }
func (realFS) SetNoCOW(string) error                 { return errLinuxOnly }
func (realFS) Truncate(string, int64) error          { return errLinuxOnly }
func (realFS) Mkfs(string) error                     { return errLinuxOnly }
func (realFS) Rename(string, string) error           { return errLinuxOnly }
func (realFS) Sync(string) error                     { return errLinuxOnly }
func (realFS) Mounted(string) (bool, error)          { return false, errLinuxOnly }
