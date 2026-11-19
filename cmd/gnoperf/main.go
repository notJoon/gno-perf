package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/notJoon/gno-perf"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := gnoperf.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, gnoperf.ErrBisectBad) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, err)
		for _, arg := range os.Args[1:] {
			if strings.EqualFold(arg, "_bisect-check") {
				os.Exit(128)
			}
		}
		os.Exit(1)
	}
}
