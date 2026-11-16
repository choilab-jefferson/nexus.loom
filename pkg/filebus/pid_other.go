//go:build !unix

package filebus

// pidAlive cannot probe a pid portably; assume alive so a live session name is never stolen.
func pidAlive(pid int) bool { return pid > 0 }
