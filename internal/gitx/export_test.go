//go:build unix

package gitx

// resetKillAll re-enables starting processes after KillAll.
func resetKillAll() {
	running.gate.Lock()
	running.stopped = false
	running.gate.Unlock()
}
