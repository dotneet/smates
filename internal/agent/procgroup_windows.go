package agent

import "os/exec"

// killGroupOnCancel keeps exec's default on Windows: the command's process is killed.
func killGroupOnCancel(*exec.Cmd) {}
