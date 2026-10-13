//go:build !windows

package main

// runIfService: вне Windows служба запускается менеджером (systemd, launchd)
// как обычный процесс, поэтому особого режима нет.
func runIfService() (handled bool, err error) { return false, nil }
