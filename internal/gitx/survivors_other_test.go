//go:build unix && !linux

package gitx

import "testing"

func checkNoSurvivors(*testing.T, string) {}
