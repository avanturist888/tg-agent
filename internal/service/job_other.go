//go:build !windows

package service

import (
	"os"
	"os/exec"
)

// killJob — на других ОС рабочий процесс просто дочерний.
type killJob struct{}

func newKillJob() (killJob, error)    { return killJob{}, nil }
func (killJob) add(*os.Process) error { return nil }
func hideWindow(*exec.Cmd)            {}
