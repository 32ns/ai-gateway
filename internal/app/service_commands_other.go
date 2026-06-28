//go:build !linux

package app

import "fmt"

func RunServiceCommand(command string, _ []string) error {
	return fmt.Errorf("%s is only supported on Linux systemd hosts", command)
}
