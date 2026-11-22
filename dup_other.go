//go:build !linux

package main

import "syscall"

// dupFD makes newfd a copy of oldfd.
func dupFD(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }
