package main

import "syscall"

func setsockoptIPTOS(fd int, tos int) error {
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS, tos)
}
