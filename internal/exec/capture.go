package exec

import (
	"context"
	"io"
	"strings"
	"sync"
)

// RunCapture runs a command to completion through an Executor and returns its
// combined stdout+stderr. It is for short command-and-check operations (e.g.
// git plumbing during workspace preparation), not for long-lived streaming
// sessions. The same call works on local and SSH executors.
func RunCapture(ctx context.Context, ex Executor, cmd Command) (string, error) {
	proc, err := ex.Start(ctx, cmd)
	if err != nil {
		return "", err
	}
	var mu sync.Mutex
	var buf strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	drain := func(r io.Reader) {
		defer wg.Done()
		b, _ := io.ReadAll(r)
		mu.Lock()
		buf.Write(b)
		mu.Unlock()
	}
	go drain(proc.Stdout())
	go drain(proc.Stderr())
	wg.Wait()
	waitErr := proc.Wait()
	return buf.String(), waitErr
}
