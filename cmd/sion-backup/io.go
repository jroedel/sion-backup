package main

import (
	"fmt"
	"io"
)

// maxStdin bounds what a secret read from stdin may be.
//
// A credential is a few dozen bytes. The bound is here so that "sion-backup
// enroll -secret-key - < /dev/urandom", typed by mistake, fails immediately
// rather than filling memory.
const maxStdin = 4 << 10

func readAll(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxStdin+1))
	if err != nil {
		return nil, err
	}

	if len(b) > maxStdin {
		return nil, fmt.Errorf("more than %d bytes; that is not a credential", maxStdin)
	}

	return b, nil
}
