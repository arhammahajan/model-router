package spaceresume

import (
	"context"
	"syscall"
	"time"
)

func syscallExec(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = time.Second
	}
	return context.WithTimeout(context.Background(), d)
}
