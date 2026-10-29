//go:build !windows

package main

import "errors"

func run(string, []string) error {
	return errors.New("berth is for Windows: elsewhere Docker Engine runs natively (Linux) or under Colima or OrbStack (macOS)")
}
