package main

import "syscall"

// dupFD makes newfd a copy of oldfd; linux/arm64 has no dup2, only dup3.
func dupFD(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }
