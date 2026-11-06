package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// promptNoDefault asks a y/N question; anything but an explicit yes declines.
func promptNoDefault(w io.Writer, r *bufio.Reader, question string) (bool, error) {
	fmt.Fprintf(w, "%s [y/N] ", question)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func promptYesDefault(w io.Writer, r io.Reader, question string) (bool, error) {
	fmt.Fprintf(w, "%s [Y/n] ", question)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if err == io.EOF && answer == "" {
		return false, nil
	}
	switch answer {
	case "", "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
